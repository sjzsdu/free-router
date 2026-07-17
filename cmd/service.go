package cmd

import (
	"encoding/json"
	"fmt"
	"os"

	servicepkg "github.com/sjzsdu/free-router/internal/service"
	"github.com/spf13/cobra"
)

func addServiceCommands(root *cobra.Command) {
	serviceCommand := &cobra.Command{
		Use:     "service",
		Aliases: []string{"daemon"},
		Short:   "Manage free-router as a user service",
	}

	serviceCommand.AddCommand(serviceAction("install", "Install and start the user service", func(command *cobra.Command, manager *servicepkg.Manager) error {
		if err := manager.Install(command.Context()); err != nil {
			return err
		}
		fmt.Printf("free-router installed to %s\nservice installed and started\n", manager.BinaryPath())
		return nil
	}))
	serviceCommand.AddCommand(serviceAction("start", "Start the installed user service", func(command *cobra.Command, manager *servicepkg.Manager) error {
		if err := manager.Start(command.Context()); err != nil {
			return err
		}
		fmt.Println("free-router service started")
		return nil
	}))
	serviceCommand.AddCommand(serviceAction("stop", "Stop the installed user service", func(command *cobra.Command, manager *servicepkg.Manager) error {
		if err := manager.Stop(command.Context()); err != nil {
			return err
		}
		fmt.Println("free-router service stopped")
		return nil
	}))
	serviceCommand.AddCommand(serviceAction("restart", "Restart the installed user service", func(command *cobra.Command, manager *servicepkg.Manager) error {
		if err := manager.Restart(command.Context()); err != nil {
			return err
		}
		fmt.Println("free-router service restarted")
		return nil
	}))
	serviceCommand.AddCommand(serviceAction("uninstall", "Stop and remove the user service", func(command *cobra.Command, manager *servicepkg.Manager) error {
		if err := manager.Uninstall(command.Context()); err != nil {
			return err
		}
		fmt.Println("free-router service uninstalled")
		return nil
	}))

	jsonOutput := false
	statusCommand := &cobra.Command{
		Use:   "status",
		Short: "Show user service status",
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
	serviceCommand.AddCommand(statusCommand)

	follow, lines := false, 100
	logsCommand := &cobra.Command{
		Use:   "logs",
		Short: "Show user service logs",
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
	serviceCommand.AddCommand(logsCommand)
	root.AddCommand(serviceCommand)
}

func serviceAction(use, short string, action func(*cobra.Command, *servicepkg.Manager) error) *cobra.Command {
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
