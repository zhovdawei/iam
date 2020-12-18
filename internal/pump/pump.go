// Copyright 2020 Lingfei Kong <colin404@foxmail.com>. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

// Package pump does all of the work necessary to create a iam pump server.
package pump

import (
	"context"
	"fmt"
	"sync"
	"time"

	cliflag "github.com/marmotedu/component-base/pkg/cli/flag"
	"github.com/marmotedu/component-base/pkg/cli/globalflag"
	"github.com/marmotedu/component-base/pkg/term"
	"github.com/marmotedu/component-base/pkg/version"
	"github.com/marmotedu/component-base/pkg/version/verflag"
	"github.com/marmotedu/errors"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	msgpack "gopkg.in/vmihailenco/msgpack.v2"

	"github.com/marmotedu/iam/pkg/log"

	genericapiserver "github.com/marmotedu/iam/internal/pkg/server"
	"github.com/marmotedu/iam/internal/pump/analytics"
	"github.com/marmotedu/iam/internal/pump/options"
	"github.com/marmotedu/iam/internal/pump/pumps"
	"github.com/marmotedu/iam/internal/pump/server"
	"github.com/marmotedu/iam/internal/pump/storage"
	"github.com/marmotedu/iam/internal/pump/storage/redis"
)

const (
	// recommendedFileName defines the configuration used by iam-pump.
	// the configuration file is different from other iam service.
	recommendedFileName = "iam-pump.yaml"

	// appName defines the executable binary filename for iam-authz-server component.
	appName = "iam-pump"
)

var analyticsStore storage.AnalyticsStorage
var pmps []pumps.Pump

// NewPumpCommand creates a *cobra.Command object with default parameters.
func NewPumpCommand() *cobra.Command {
	cliflag.InitFlags()

	s := options.NewPumpOptions()

	cmd := &cobra.Command{
		Use:   appName,
		Short: "IAM Pump is a pluggable analytics purger to move Analytics generated by your iam nodes to any back-end.",
		Long: `IAM Pump is a pluggable analytics purger to move Analytics generated by your iam nodes to any back-end.

Find more iam-pump information at:
    https://github.com/marmotedu/iam/blob/master/docs/admin/iam-pump.md`,

		// stop printing usage when the command errors
		SilenceUsage: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			verflag.PrintAndExitIfRequested()
			cliflag.PrintFlags(cmd.Flags())

			if err := viper.BindPFlags(cmd.Flags()); err != nil {
				return err
			}

			// set default options
			completedOptions, err := Complete(s)
			if err != nil {
				return err
			}

			// validate options
			if errs := completedOptions.Validate(); len(errs) != 0 {
				return errors.NewAggregate(errs)
			}

			// setup logger
			log.InitWithOptions(completedOptions.Log)
			defer log.Flush()

			return Run(completedOptions, genericapiserver.SetupSignalHandler())
		},
		PersistentPostRunE: func(cmd *cobra.Command, args []string) error {
			return nil
		},
		Args: func(cmd *cobra.Command, args []string) error {
			for _, arg := range args {
				if len(arg) > 0 {
					return fmt.Errorf("%q does not take any arguments, got %q", cmd.CommandPath(), args)
				}
			}
			return nil
		},
	}

	namedFlagSets := s.Flags()
	verflag.AddFlags(namedFlagSets.FlagSet("global"))
	globalflag.AddGlobalFlags(namedFlagSets.FlagSet("global"), cmd.Name())
	fs := cmd.Flags()
	for _, f := range namedFlagSets.FlagSets {
		fs.AddFlagSet(f)
	}

	usageFmt := "Usage:\n  %s\n"
	cols, _, _ := term.TerminalSize(cmd.OutOrStdout())
	cmd.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		fmt.Fprintf(cmd.OutOrStdout(), "%s\n\n"+usageFmt, cmd.Long, cmd.UseLine())
		cliflag.PrintSections(cmd.OutOrStdout(), namedFlagSets, cols)
	})
	cmd.SetUsageFunc(func(cmd *cobra.Command) error {
		fmt.Fprintf(cmd.OutOrStderr(), usageFmt, cmd.UseLine())
		cliflag.PrintSections(cmd.OutOrStderr(), namedFlagSets, cols)
		return nil
	})

	return cmd
}

// Run runs the specified pump server. This should never exit.
func Run(completedOptions completedPumpOptions, stopCh <-chan struct{}) error {
	// To help debugging, immediately log config and version
	log.Infof("config: `%s`", completedOptions.String())
	log.Infof("version: %+v", version.Get().ToJSON())

	if err := completedOptions.Init(); err != nil {
		return err
	}

	go server.ServeHealthCheck(completedOptions.HealthCheckPath, completedOptions.HealthCheckAddress)

	// start the worker loop
	log.Infof("Starting purge loop @%d%s", completedOptions.PurgeDelay, "(s)")

	StartPurgeLoop(completedOptions.PurgeDelay, completedOptions.OmitDetailedRecording)
	return nil
}

// completedPumpOptions is a private wrapper that enforces a call of complete() before Run can be invoked.
type completedPumpOptions struct {
	*options.PumpOptions
}

// Complete completes the PumpOptions with provided PumpOptions returning completedPumpOptions.
func Complete(s *options.PumpOptions) (completedPumpOptions, error) {
	var options completedPumpOptions

	genericapiserver.LoadConfig(s.PumpConfig, recommendedFileName)

	if err := viper.Unmarshal(s); err != nil {
		return options, err
	}

	options.PumpOptions = s

	return options, nil
}

func setupAnalyticsStore(completedOptions completedPumpOptions) error {
	analyticsStore = &redis.RedisClusterStorageManager{}
	return analyticsStore.Init(completedOptions.RedisOptions)
}

func (completedOptions completedPumpOptions) Init() error {
	// Create the store
	if err := setupAnalyticsStore(completedOptions); err != nil {
		return err
	}

	// prime the pumps
	initialisePumps(completedOptions)

	return nil
}

func initialisePumps(completedOptions completedPumpOptions) {
	pmps = make([]pumps.Pump, len(completedOptions.Pumps))
	i := 0
	for key, pmp := range completedOptions.Pumps {
		pumpTypeName := pmp.Type
		if pumpTypeName == "" {
			pumpTypeName = key
		}

		pmpType, err := pumps.GetPumpByName(pumpTypeName)
		if err != nil {
			log.Errorf("Pump load error (skipping): %s", err.Error())
		} else {
			thisPmp := pmpType.New()
			initErr := thisPmp.Init(pmp.Meta)
			if initErr != nil {
				log.Errorf("Pump init error (skipping): %s", initErr.Error())
			} else {
				log.Infof("Init Pump: %s", thisPmp.GetName())
				thisPmp.SetFilters(pmp.Filters)
				thisPmp.SetTimeout(pmp.Timeout)
				thisPmp.SetOmitDetailedRecording(pmp.OmitDetailedRecording)
				pmps[i] = thisPmp
			}
		}
		i++
	}
}

// StartPurgeLoop start a loop to moves the data to any back-end.
func StartPurgeLoop(secInterval int, omitDetails bool) {
	for range time.Tick(time.Duration(secInterval) * time.Second) {
		analyticsValues := analyticsStore.GetAndDeleteSet(storage.AnalyticsKeyName)
		if len(analyticsValues) > 0 {
			// Convert to something clean
			keys := make([]interface{}, len(analyticsValues))

			for i, v := range analyticsValues {
				decoded := analytics.AnalyticsRecord{}
				err := msgpack.Unmarshal([]byte(v.(string)), &decoded)
				log.Debugf("Decoded Record: %v", decoded)
				if err != nil {
					log.Errorf("Couldn't unmarshal analytics data: %s", err.Error())
				} else {
					if omitDetails {
						decoded.Policies = ""
						decoded.Deciders = ""
					}
					keys[i] = interface{}(decoded)
				}
			}

			// Send to pumps
			writeToPumps(keys, secInterval)
		}
	}
}

func writeToPumps(keys []interface{}, purgeDelay int) {
	// Send to pumps
	if pmps != nil {
		var wg sync.WaitGroup
		wg.Add(len(pmps))
		for _, pmp := range pmps {
			go execPumpWriting(&wg, pmp, &keys, purgeDelay)
		}
		wg.Wait()
	} else {
		log.Warn("No pumps defined!")
	}
}

func filterData(pump pumps.Pump, keys []interface{}) []interface{} {
	filters := pump.GetFilters()
	if !filters.HasFilter() && !pump.GetOmitDetailedRecording() {
		return keys
	}
	filteredKeys := keys[:] // nolint: gocritic
	newLenght := 0

	for _, key := range filteredKeys {
		decoded := key.(analytics.AnalyticsRecord)
		if pump.GetOmitDetailedRecording() {
			decoded.Policies = ""
			decoded.Deciders = ""
		}
		if filters.ShouldFilter(decoded) {
			continue
		}
		filteredKeys[newLenght] = decoded
		newLenght++
	}
	filteredKeys = filteredKeys[:newLenght]
	return filteredKeys
}

func execPumpWriting(wg *sync.WaitGroup, pmp pumps.Pump, keys *[]interface{}, purgeDelay int) {
	timer := time.AfterFunc(time.Duration(purgeDelay)*time.Second, func() {
		if pmp.GetTimeout() == 0 {
			log.Warnf("Pump %s is taking more time than the value configured of purge_delay. You should try to set a timeout for this pump.", pmp.GetName())
		} else if pmp.GetTimeout() > purgeDelay {
			log.Warnf("Pump %s is taking more time than the value configured of purge_delay. You should try lowering the timeout configured for this pump.", pmp.GetName())
		}
	})
	defer timer.Stop()
	defer wg.Done()

	log.Debugf("Writing to: %s", pmp.GetName())

	ch := make(chan error, 1)
	// Load pump timeout
	timeout := pmp.GetTimeout()
	var ctx context.Context
	var cancel context.CancelFunc
	// Initialize context depending if the pump has a configured timeout
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	} else {
		ctx, cancel = context.WithCancel(context.Background())
	}

	defer cancel()

	go func(ch chan error, ctx context.Context, pmp pumps.Pump, keys *[]interface{}) {
		filteredKeys := filterData(pmp, *keys)

		ch <- pmp.WriteData(ctx, filteredKeys)
	}(ch, ctx, pmp, keys)

	select {
	case err := <-ch:
		if err != nil {
			log.Warnf("Error Writing to: %s - Error: %s", pmp.GetName(), err.Error())
		}
	case <-ctx.Done():
		switch ctx.Err() {
		case context.Canceled:
			log.Warnf("The writing to %s have got canceled.", pmp.GetName())
		case context.DeadlineExceeded:
			log.Warnf("Timeout Writing to: %s", pmp.GetName())
		}
	}
}
