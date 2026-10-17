package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sjzsdu/free-router/internal/appdirs"
	"github.com/sjzsdu/free-router/internal/doctor"
	"github.com/spf13/cobra"
)

func addDoctorCommand(root *cobra.Command, opts *options) {
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose configuration, credentials, service health and recent failures",
		Long: "Run a read-only diagnosis of this installation.\n\n" +
			"It checks the route configuration, which providers have credentials, local state files,\n" +
			"whether the service answers, why its most recent requests failed, and how big the\n" +
			"daemon log has grown. Every finding carries the next step to take. Nothing is\n" +
			"modified and no model is called.",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			report := doctor.Run(command.Context(), doctor.Options{
				Addr:            opts.addr,
				ConfigPath:      opts.config,
				FreeModels:      opts.freeModels,
				CredentialsPath: opts.credentials,
				HealthPath:      opts.health,
				StatisticsPath:  opts.statistics,
				LogPath:         daemonLogPath(),
				AdminToken:      os.Getenv("FREE_ROUTER_ADMIN_TOKEN"),
			})
			if jsonOutput {
				if err := json.NewEncoder(command.OutOrStdout()).Encode(report); err != nil {
					return err
				}
				if report.Failed() {
					return errors.New("doctor found problems that prevent serving requests")
				}
				return nil
			}
			out := command.OutOrStdout()
			counts := map[doctor.Status]int{}
			for _, check := range report.Checks {
				counts[check.Status]++
				fmt.Fprintf(out, "%-4s %s: %s\n", check.Status, check.Name, check.Detail)
				if check.Hint != "" {
					fmt.Fprintf(out, "     hint: %s\n", check.Hint)
				}
			}
			fmt.Fprintf(out, "\n%d ok, %d warning(s), %d failed, %d skipped\n",
				counts[doctor.StatusOK], counts[doctor.StatusWarn], counts[doctor.StatusFail], counts[doctor.StatusSkip])
			if report.Failed() {
				return errors.New("doctor found problems that prevent serving requests")
			}
			return nil
		},
	}
	command.Flags().BoolVar(&jsonOutput, "json", false, "print the report as JSON")
	root.AddCommand(command)
}

// daemonLogPath is where the background service writes its stdout/stderr on
// platforms that use a file (macOS). On systemd the unit writes to the
// journal, so the path simply will not exist and the check says so instead of
// pretending the installation is broken.
func daemonLogPath() string {
	return filepath.Join(appdirs.Default(), "free-router.log")
}
