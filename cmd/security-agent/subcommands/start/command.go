// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

// Package start implements start related subcommands
package start

import (
	"context"
	"errors"
	_ "expvar" // Blank import used because this isn't directly used in this file
	"fmt"
	"net/http"
	_ "net/http/pprof" // Blank import used because this isn't directly used in this file
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/fx"

	"github.com/DataDog/datadog-agent/cmd/manager"
	"github.com/DataDog/datadog-agent/cmd/security-agent/api"
	"github.com/DataDog/datadog-agent/cmd/security-agent/command"
	"github.com/DataDog/datadog-agent/cmd/security-agent/flags"
	"github.com/DataDog/datadog-agent/cmd/security-agent/subcommands/compliance"
	"github.com/DataDog/datadog-agent/cmd/security-agent/subcommands/runtime"
	"github.com/DataDog/datadog-agent/comp/aggregator/demultiplexer"
	"github.com/DataDog/datadog-agent/comp/aggregator/demultiplexer/demultiplexerimpl"
	"github.com/DataDog/datadog-agent/comp/core"
	"github.com/DataDog/datadog-agent/comp/core/config"
	"github.com/DataDog/datadog-agent/comp/core/log"
	"github.com/DataDog/datadog-agent/comp/core/log/logimpl"
	"github.com/DataDog/datadog-agent/comp/core/secrets"
	"github.com/DataDog/datadog-agent/comp/core/status"
	"github.com/DataDog/datadog-agent/comp/core/status/statusimpl"
	"github.com/DataDog/datadog-agent/comp/core/sysprobeconfig"
	"github.com/DataDog/datadog-agent/comp/core/sysprobeconfig/sysprobeconfigimpl"
	"github.com/DataDog/datadog-agent/comp/core/tagger"
	"github.com/DataDog/datadog-agent/comp/core/telemetry"
	"github.com/DataDog/datadog-agent/comp/core/workloadmeta"
	"github.com/DataDog/datadog-agent/comp/core/workloadmeta/collectors"
	"github.com/DataDog/datadog-agent/comp/dogstatsd"
	"github.com/DataDog/datadog-agent/comp/dogstatsd/statsd"
	"github.com/DataDog/datadog-agent/comp/forwarder"
	"github.com/DataDog/datadog-agent/comp/forwarder/defaultforwarder"
	"github.com/DataDog/datadog-agent/comp/forwarder/eventplatform/eventplatformimpl"
	"github.com/DataDog/datadog-agent/comp/forwarder/eventplatformreceiver/eventplatformreceiverimpl"
	orchestratorForwarderImpl "github.com/DataDog/datadog-agent/comp/forwarder/orchestrator/orchestratorimpl"
	"github.com/DataDog/datadog-agent/comp/metadata/host/hostimpl"
	"github.com/DataDog/datadog-agent/pkg/collector/python"
	pkgCompliance "github.com/DataDog/datadog-agent/pkg/compliance"
	pkgconfig "github.com/DataDog/datadog-agent/pkg/config"
	"github.com/DataDog/datadog-agent/pkg/config/model"
	"github.com/DataDog/datadog-agent/pkg/config/settings"
	"github.com/DataDog/datadog-agent/pkg/config/setup"
	"github.com/DataDog/datadog-agent/pkg/pidfile"
	"github.com/DataDog/datadog-agent/pkg/security/agent"
	"github.com/DataDog/datadog-agent/pkg/security/utils"
	"github.com/DataDog/datadog-agent/pkg/status/health"
	"github.com/DataDog/datadog-agent/pkg/util"
	"github.com/DataDog/datadog-agent/pkg/util/fxutil"
	"github.com/DataDog/datadog-agent/pkg/util/profiling"
	"github.com/DataDog/datadog-agent/pkg/util/startstop"
	"github.com/DataDog/datadog-agent/pkg/version"
	ddgostatsd "github.com/DataDog/datadog-go/v5/statsd"
)

type cliParams struct {
	*command.GlobalParams

	pidfilePath string
}

// Commands returns the start commands
func Commands(globalParams *command.GlobalParams) []*cobra.Command {
	params := &cliParams{
		GlobalParams: globalParams,
	}

	startCmd := &cobra.Command{
		Use:   "start",
		Short: "Start the Security Agent",
		Long:  `Runs Datadog Security agent in the foreground`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// TODO: Similar to the agent itself, once the security agent is represented as a component, and not a function (start),
			// this will use `fxutil.Run` instead of `fxutil.OneShot`.

			// note that any changes to the arguments to OneShot need to be reflected into
			// the service initialization in ../../main_windows.go
			return fxutil.OneShot(start,
				fx.Supply(params),
				fx.Supply(core.BundleParams{
					ConfigParams:         config.NewSecurityAgentParams(params.ConfigFilePaths),
					SysprobeConfigParams: sysprobeconfigimpl.NewParams(sysprobeconfigimpl.WithSysProbeConfFilePath(globalParams.SysProbeConfFilePath)),
					SecretParams:         secrets.NewEnabledParams(),
					LogParams:            logimpl.ForDaemon(command.LoggerName, "security_agent.log_file", setup.DefaultSecurityAgentLogFile),
				}),
				core.Bundle(),
				dogstatsd.ClientBundle,
				forwarder.Bundle(),
				fx.Provide(defaultforwarder.NewParamsWithResolvers),
				demultiplexerimpl.Module(),
				orchestratorForwarderImpl.Module(),
				fx.Supply(orchestratorForwarderImpl.NewDisabledParams()),
				eventplatformimpl.Module(),
				fx.Supply(eventplatformimpl.NewDisabledParams()),
				eventplatformreceiverimpl.Module(),
				fx.Supply(demultiplexerimpl.NewDefaultParams()),
				// workloadmeta setup
				collectors.GetCatalog(),
				workloadmeta.Module(),
				fx.Provide(func(config config.Component) workloadmeta.Params {

					catalog := workloadmeta.NodeAgent

					if config.GetBool("security_agent.remote_workloadmeta") {
						catalog = workloadmeta.Remote
					}

					return workloadmeta.Params{
						AgentType: catalog,
					}
				}),
				tagger.Module(),
				fx.Provide(func(config config.Component) tagger.Params {
					if config.GetBool("security_agent.remote_tagger") {
						return tagger.NewNodeRemoteTaggerParams()
					}
					return tagger.NewTaggerParams()
				}),
				fx.Provide(func() startstop.Stopper {
					return startstop.NewSerialStopper()
				}),
				fx.Provide(func(config config.Component, statsd statsd.Component) (ddgostatsd.ClientInterface, error) {
					return statsd.CreateForHostPort(setup.GetBindHost(config), config.GetInt("dogstatsd_port"))
				}),
				fx.Provide(func(stopper startstop.Stopper, log log.Component, config config.Component, statsdClient ddgostatsd.ClientInterface, demultiplexer demultiplexer.Component) (status.InformationProvider, *agent.RuntimeSecurityAgent, error) {
					hostnameDetected, err := utils.GetHostnameWithContextAndFallback(context.TODO())
					if err != nil {
						return status.NewInformationProvider(nil), nil, err
					}

					runtimeAgent, err := runtime.StartRuntimeSecurity(log, config, hostnameDetected, stopper, statsdClient, demultiplexer)
					if err != nil {
						return status.NewInformationProvider(nil), nil, err
					}

					if runtimeAgent == nil {
						return status.NewInformationProvider(nil), nil, nil
					}

					// TODO - components: Do not remove runtimeAgent ref until "github.com/DataDog/datadog-agent/pkg/security/agent" is a component so they're not GCed
					return status.NewInformationProvider(runtimeAgent.StatusProvider()), runtimeAgent, nil
				}),
				fx.Provide(func(stopper startstop.Stopper, log log.Component, config config.Component, statsdClient ddgostatsd.ClientInterface, demultiplexer demultiplexer.Component, sysprobeconfig sysprobeconfig.Component) (status.InformationProvider, *pkgCompliance.Agent, error) {
					hostnameDetected, err := utils.GetHostnameWithContextAndFallback(context.TODO())
					if err != nil {
						return status.NewInformationProvider(nil), nil, err
					}

					// start compliance security agent
					complianceAgent, err := compliance.StartCompliance(log, config, sysprobeconfig, hostnameDetected, stopper, statsdClient, demultiplexer)
					if err != nil {
						return status.NewInformationProvider(nil), nil, err
					}

					if complianceAgent == nil {
						return status.NewInformationProvider(nil), nil, nil
					}

					// TODO - components: Do not remove complianceAgent ref until "github.com/DataDog/datadog-agent/pkg/compliance" is a component so they're not GCed
					return status.NewInformationProvider(complianceAgent.StatusProvider()), complianceAgent, nil
				}),
				fx.Supply(
					status.Params{
						PythonVersionGetFunc: python.GetPythonVersion,
					},
				),
				fx.Provide(func(config config.Component) status.HeaderInformationProvider {
					return status.NewHeaderInformationProvider(hostimpl.StatusProvider{
						Config: config,
					})
				}),
				statusimpl.Module(),
			)
		},
	}

	startCmd.Flags().StringVarP(&params.pidfilePath, flags.PidFile, "p", "", "path to the pidfile")

	return []*cobra.Command{startCmd}
}

// start will start the security-agent.
//
// TODO(components): note how workloadmeta is passed anonymously, it is still required as it is used
// as a global. This should eventually be fixed and all workloadmeta interactions should be via the
// injected instance.
func start(log log.Component, config config.Component, _ secrets.Component, _ statsd.Component, _ sysprobeconfig.Component, telemetry telemetry.Component, _ workloadmeta.Component, demultiplexer demultiplexer.Component, params *cliParams, statusComponent status.Component) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer StopAgent(cancel, log)

	err := RunAgent(ctx, log, config, telemetry, params.pidfilePath, demultiplexer, statusComponent)
	if errors.Is(err, errAllComponentsDisabled) || errors.Is(err, errNoAPIKeyConfigured) {
		return nil
	}
	if err != nil {
		return err
	}

	stopCh := make(chan struct{})
	defer close(stopCh)
	go handleSignals(log, stopCh)

	// Block here until we receive a stop signal
	<-stopCh

	return nil
}

// handleSignals handles OS signals, and sends a message on stopCh when an interrupt
// signal is received.
func handleSignals(log log.Component, stopCh chan struct{}) {
	// Setup a channel to catch OS signals
	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, os.Interrupt, syscall.SIGTERM, syscall.SIGPIPE)

	// Block here until we receive the interrupt signal
	for signo := range signalCh {
		switch signo {
		case syscall.SIGPIPE:
			// By default, systemd redirects the stdout to journald. When journald is stopped or crashes we receive a SIGPIPE signal.
			// Go ignores SIGPIPE signals unless it is when stdout or stdout is closed, in this case the agent is stopped.
			// We never want dogstatsd to stop upon receiving SIGPIPE, so we intercept the SIGPIPE signals and just discard them.
		default:
			log.Infof("Received signal '%s', shutting down...", signo)

			stopCh <- struct{}{}
			return
		}
	}
}

var (
	stopper      startstop.Stopper
	srv          *api.Server
	expvarServer *http.Server
)

var errAllComponentsDisabled = errors.New("all security-agent component are disabled")
var errNoAPIKeyConfigured = errors.New("no API key configured")

// RunAgent initialized resources and starts API server
func RunAgent(ctx context.Context, log log.Component, config config.Component, telemetry telemetry.Component, pidfilePath string, demultiplexer demultiplexer.Component, statusComponent status.Component) (err error) {
	if err := util.SetupCoreDump(config); err != nil {
		log.Warnf("Can't setup core dumps: %v, core dumps might not be available after a crash", err)
	}

	if pidfilePath != "" {
		err = pidfile.WritePID(pidfilePath)
		if err != nil {
			return log.Errorf("Error while writing PID file, exiting: %v", err)
		}
		defer os.Remove(pidfilePath)
		log.Infof("pid '%d' written to pid file '%s'", os.Getpid(), pidfilePath)
	}

	// Check if we have at least one component to start based on config
	if !config.GetBool("compliance_config.enabled") && !config.GetBool("runtime_security_config.enabled") {
		log.Infof("All security-agent components are deactivated, exiting")

		// A sleep is necessary so that sysV doesn't think the agent has failed
		// to startup because of an error. Only applies on Debian 7.
		time.Sleep(5 * time.Second)

		return errAllComponentsDisabled
	}

	if !config.IsSet("api_key") {
		log.Critical("No API key configured, exiting")

		// A sleep is necessary so that sysV doesn't think the agent has failed
		// to startup because of an error. Only applies on Debian 7.
		time.Sleep(5 * time.Second)

		return errNoAPIKeyConfigured
	}

	err = manager.ConfigureAutoExit(ctx, config)
	if err != nil {
		log.Criticalf("Unable to configure auto-exit, err: %w", err)
		return nil
	}

	// Setup expvar server
	port := config.GetString("security_agent.expvar_port")
	pkgconfig.Datadog.Set("expvar_port", port, model.SourceAgentRuntime)
	if config.GetBool("telemetry.enabled") {
		http.Handle("/telemetry", telemetry.Handler())
	}
	expvarServer := &http.Server{
		Addr:    "127.0.0.1:" + port,
		Handler: http.DefaultServeMux,
	}
	go func() {
		err := expvarServer.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			log.Errorf("Error creating expvar server on port %v: %v", port, err)
		}
	}()

	demultiplexer.AddAgentStartupTelemetry(fmt.Sprintf("%s - Datadog Security Agent", version.AgentVersion))

	if err = initRuntimeSettings(); err != nil {
		return err
	}

	srv, err = api.NewServer(statusComponent)
	if err != nil {
		return log.Errorf("Error while creating api server, exiting: %v", err)
	}

	if err = srv.Start(); err != nil {
		return log.Errorf("Error while starting api server, exiting: %v", err)
	}

	if err := setupInternalProfiling(config); err != nil {
		return log.Errorf("Error while setuping internal profiling, exiting: %v", err)
	}

	log.Infof("Datadog Security Agent is now running.")

	return
}

func initRuntimeSettings() error {
	return settings.RegisterRuntimeSetting(settings.NewLogLevelRuntimeSetting())
}

// StopAgent stops the API server and clean up resources
func StopAgent(cancel context.CancelFunc, log log.Component) {
	// retrieve the agent health before stopping the components
	// GetReadyNonBlocking has a 100ms timeout to avoid blocking
	healthStatus, err := health.GetReadyNonBlocking()
	if err != nil {
		log.Warnf("Security Agent health unknown: %s", err)
	} else if len(healthStatus.Unhealthy) > 0 {
		log.Warnf("Some components were unhealthy: %v", healthStatus.Unhealthy)
	}

	// gracefully shut down any component
	cancel()

	// stop metaScheduler and statsd if they are instantiated
	if stopper != nil {
		stopper.Stop()
	}

	if srv != nil {
		srv.Stop()
	}
	if expvarServer != nil {
		if err := expvarServer.Shutdown(context.Background()); err != nil {
			log.Errorf("Error shutting down expvar server: %v", err)
		}
	}

	log.Info("See ya!")
}

func setupInternalProfiling(config config.Component) error {
	if config.GetBool(secAgentKey("internal_profiling.enabled")) {
		v, _ := version.Agent()

		cfgSite := config.GetString(secAgentKey("internal_profiling.site"))
		cfgURL := config.GetString(secAgentKey("security_agent.internal_profiling.profile_dd_url"))

		// check if TRACE_AGENT_URL is set, in which case, forward the profiles to the trace agent
		var site string
		if traceAgentURL := os.Getenv("TRACE_AGENT_URL"); len(traceAgentURL) > 0 {
			site = fmt.Sprintf(profiling.ProfilingLocalURLTemplate, traceAgentURL)
		} else {
			site = fmt.Sprintf(profiling.ProfilingURLTemplate, cfgSite)
			if cfgURL != "" {
				site = cfgURL
			}
		}

		tags := config.GetStringSlice(secAgentKey("internal_profiling.extra_tags"))
		tags = append(tags, fmt.Sprintf("version:%v", v))

		profSettings := profiling.Settings{
			ProfilingURL:         site,
			Env:                  config.GetString(secAgentKey("internal_profiling.env")),
			Service:              "security-agent",
			Period:               config.GetDuration(secAgentKey("internal_profiling.period")),
			CPUDuration:          config.GetDuration(secAgentKey("internal_profiling.cpu_duration")),
			MutexProfileFraction: config.GetInt(secAgentKey("internal_profiling.mutex_profile_fraction")),
			BlockProfileRate:     config.GetInt(secAgentKey("internal_profiling.block_profile_rate")),
			WithGoroutineProfile: config.GetBool(secAgentKey("internal_profiling.enable_goroutine_stacktraces")),
			WithDeltaProfiles:    config.GetBool(secAgentKey("internal_profiling.delta_profiles")),
			Socket:               config.GetString(secAgentKey("internal_profiling.unix_socket")),
			Tags:                 tags,
		}

		return profiling.Start(profSettings)
	}

	return nil
}

func secAgentKey(sub string) string {
	return fmt.Sprintf("security_agent.%s", sub)
}
