package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"github.com/sjzsdu/free-router/internal/provider"
	servicepkg "github.com/sjzsdu/free-router/internal/service"
	"github.com/spf13/cobra"
)

// daemonEnvironment snapshots every setting the background service needs.
//
// launchd and systemd start the daemon with an empty environment, so anything
// read from the environment in the foreground (provider keys, tokens, paths,
// the attempt budget, log settings) has to be copied here explicitly. Settings
// that were missing from this snapshot used to disappear silently the moment
// the user installed the daemon.
func daemonEnvironment(opts options) (map[string]string, error) {
	envMap, err := configuredEnvMap(opts)
	if err != nil {
		return nil, err
	}
	environment := make(map[string]string)
	for _, name := range provider.EnvironmentNames(envMap) {
		if value := os.Getenv(name); value != "" {
			environment[name] = value
		}
	}
	environment["FREE_ROUTER_ADDR"] = opts.addr
	environment["FREE_ROUTER_CACHE"] = opts.cache
	if opts.freeModels != "" {
		environment["FREE_ROUTER_FREE_MODELS"] = opts.freeModels
	}
	environment["FREE_ROUTER_CONFIG"] = opts.config
	environment["FREE_ROUTER_CREDENTIALS"] = opts.credentials
	environment["FREE_ROUTER_HEALTH"] = opts.health
	environment["FREE_ROUTER_STATISTICS"] = opts.statistics
	environment["FREE_ROUTER_MAX_ATTEMPTS"] = strconv.Itoa(opts.maxAttempts)
	if opts.apiToken != "" {
		environment["FREE_ROUTER_API_TOKEN"] = opts.apiToken
	}
	if opts.adminToken != "" {
		environment["FREE_ROUTER_ADMIN_TOKEN"] = opts.adminToken
	}
	if opts.adminAllowRemote {
		environment["FREE_ROUTER_ADMIN_ALLOW_REMOTE"] = "true"
	}
	// Logging settings must survive the jump into launchd/systemd, otherwise
	// "I raised the level" silently does nothing in daemon mode.
	environment["FREE_ROUTER_LOG_LEVEL"] = opts.logLevel
	environment["FREE_ROUTER_LOG_FORMAT"] = opts.logFormat
	if opts.providers != "" {
		environment["FREE_ROUTER_PROVIDERS"] = opts.providers
	}
	return environment, nil
}

func addDaemonCommands(root *cobra.Command, opts *options) {
	daemonCommand := &cobra.Command{
		Use:   "daemon",
		Short: "Manage free-router as a background daemon",
	}

	daemonCommand.AddCommand(daemonAction("install", "Install and start the background daemon", func(command *cobra.Command, manager *servicepkg.Manager) error {
		environment, err := daemonEnvironment(*opts)
		if err != nil {
			return err
		}
		if err := manager.Install(command.Context(), environment); err != nil {
			return err
		}
		fmt.Printf("free-router installed to %s\ndaemon installed and started\n", manager.BinaryPath())
		if hint := manager.LogHint(); hint != "" {
			fmt.Printf("logs: %s\n", hint)
		}
		return nil
	}))
	daemonCommand.AddCommand(daemonAction("start", "Start the installed daemon", func(command *cobra.Command, manager *servicepkg.Manager) error {
		if err := manager.Start(command.Context()); err != nil {
			return err
		}
		fmt.Println("free-router daemon started")
		return nil
	}))
	daemonCommand.AddCommand(daemonAction("stop", "Stop the installed daemon", func(command *cobra.Command, manager *servicepkg.Manager) error {
		if err := manager.Stop(command.Context()); err != nil {
			return err
		}
		fmt.Println("free-router daemon stopped")
		return nil
	}))
	daemonCommand.AddCommand(daemonAction("restart", "Restart the installed daemon", func(command *cobra.Command, manager *servicepkg.Manager) error {
		if err := manager.Restart(command.Context()); err != nil {
			return err
		}
		fmt.Println("free-router daemon restarted")
		return nil
	}))
	daemonCommand.AddCommand(daemonAction("uninstall", "Stop and remove the daemon", func(command *cobra.Command, manager *servicepkg.Manager) error {
		if err := manager.Uninstall(command.Context()); err != nil {
			return err
		}
		fmt.Println("free-router daemon uninstalled")
		return nil
	}))

	jsonOutput := false
	statusCommand := &cobra.Command{
		Use:   "status",
		Short: "Show daemon status",
		RunE: func(command *cobra.Command, _ []string) error {
			manager, err := servicepkg.New()
			if err != nil {
				return err
			}
			status, err := manager.Status(command.Context())
			if err != nil {
				return err
			}
			if jsonOutput {
				return json.NewEncoder(os.Stdout).Encode(status)
			}
			state := "stopped"
			if status.Running {
				state = "running"
			}
			fmt.Printf("status: %s\nmanager: %s\ninstalled: %t\n", state, status.Manager, status.Installed)
			fmt.Printf("binary: %s\n", status.Binary)
			if status.PID > 0 {
				fmt.Printf("pid: %d\n", status.PID)
			}
			return nil
		},
	}
	statusCommand.Flags().BoolVar(&jsonOutput, "json", false, "print status as JSON")
	daemonCommand.AddCommand(statusCommand)

	follow, lines := false, 100
	logsCommand := &cobra.Command{
		Use:   "logs",
		Short: "Show daemon logs",
		RunE: func(command *cobra.Command, _ []string) error {
			manager, err := servicepkg.New()
			if err != nil {
				return err
			}
			return manager.Logs(command.Context(), os.Stdout, follow, lines)
		},
	}
	logsCommand.Flags().BoolVarP(&follow, "follow", "f", false, "follow new log entries")
	logsCommand.Flags().IntVarP(&lines, "lines", "n", lines, "number of recent lines")
	daemonCommand.AddCommand(logsCommand)
	root.AddCommand(daemonCommand)
}

func daemonAction(use, short string, action func(*cobra.Command, *servicepkg.Manager) error) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		RunE: func(command *cobra.Command, _ []string) error {
			manager, err := servicepkg.New()
			if err != nil {
				return err
			}
			return action(command, manager)
		},
	}
}
