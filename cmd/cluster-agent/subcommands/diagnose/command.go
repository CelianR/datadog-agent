// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build !windows && kubeapiserver

// Package diagnose implements 'cluster-agent diagnose'.
package diagnose

import (
	"github.com/fatih/color"
	"github.com/spf13/cobra"
	"go.uber.org/fx"

	"github.com/DataDog/datadog-agent/cmd/cluster-agent/command"
	"github.com/DataDog/datadog-agent/comp/aggregator/diagnosesendermanager"
	"github.com/DataDog/datadog-agent/comp/aggregator/diagnosesendermanager/diagnosesendermanagerimpl"
	"github.com/DataDog/datadog-agent/comp/collector/collector"
	"github.com/DataDog/datadog-agent/comp/core"
	"github.com/DataDog/datadog-agent/comp/core/config"
	"github.com/DataDog/datadog-agent/comp/core/log/logimpl"
	"github.com/DataDog/datadog-agent/comp/core/secrets"
	"github.com/DataDog/datadog-agent/pkg/diagnose"
	"github.com/DataDog/datadog-agent/pkg/diagnose/diagnosis"
	"github.com/DataDog/datadog-agent/pkg/util/fxutil"
	"github.com/DataDog/datadog-agent/pkg/util/optional"
)

// Commands returns a slice of subcommands for the 'cluster-agent' command.
func Commands(globalParams *command.GlobalParams) []*cobra.Command {
	cmd := &cobra.Command{
		Use:   "diagnose",
		Short: "Execute some connectivity diagnosis on your system",
		Long:  ``,
		RunE: func(cmd *cobra.Command, args []string) error {
			return fxutil.OneShot(run,
				fx.Supply(core.BundleParams{
					ConfigParams: config.NewClusterAgentParams(globalParams.ConfFilePath),
					SecretParams: secrets.NewEnabledParams(),
					LogParams:    logimpl.ForOneShot(command.LoggerName, "off", true), // no need to show regular logs
				}),
				core.Bundle(),
				diagnosesendermanagerimpl.Module(),
			)
		},
	}

	return []*cobra.Command{cmd}
}

func run(diagnoseSenderManager diagnosesendermanager.Component, secretResolver secrets.Component) error {
	// Verbose:  true - to show details like if was done a while ago
	// RunLocal: true - do not attept to run in actual running agent but
	//                  may need to implement it in future
	// Include: connectivity-datadog-autodiscovery - limit to a single
	//                  diagnose suite as it was done in this agent for
	//                  a while. Most likely need to relax or add more
	//                  diagnose suites in the future
	diagCfg := diagnosis.Config{
		Verbose:  true, // show details
		RunLocal: true, // do not attept to run in actual runnin agent (may need to implement it in future)
		Include:  []string{"connectivity-datadog-autodiscovery"},
	}
	diagnoseDeps := diagnose.NewSuitesDeps(diagnoseSenderManager, optional.NewNoneOption[collector.Component](), secretResolver)

	return diagnose.RunStdOut(color.Output, diagCfg, diagnoseDeps)
}
