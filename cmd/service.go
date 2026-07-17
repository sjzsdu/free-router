package cmd

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/sjzsdu/free-router/internal/provider"
	servicepkg "github.com/sjzsdu/free-router/internal/service"
	"github.com/spf13/cobra"
)

func addDaemonCommands(root *cobra.Command, opts *options) {
	daemonCommand := &cobra.Command{
		Use:   "daemon",
		Short: "Manage free-router as a background daemon",
	}

	daemonCommand.AddCommand(daemonAction("install", "Install and start the background daemon", func(command *cobra.Command, manager *servicepkg.Manager) error {
		envMap, err := configuredEnvMap(*opts)
		if err != nil {
			return err
		}
		environment := make(map[string]string)
		for _, name := range provider.EnvironmentNames(envMap) {
			if value := os.Getenv(name); value != "" {
				environment[name] = value
			}
		}
		environment["FREE_ROUTER_ADDR"] = opts.addr
		environment["FREE_ROUTER_CONFIG"] = opts.config
		environment["FREE_ROUTER_CREDENTIALS"] = opts.credentials
		if opts.providers != "" {
			environment["FREE_ROUTER_PROVIDERS"] = opts.providers
		}
		if err := manager.Install(command.Context(), environment); err != nil {
			return err
		}
		fmt.Printf("free-router installed to %s\ndaemon installed and started\n", manager.BinaryPath())
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
