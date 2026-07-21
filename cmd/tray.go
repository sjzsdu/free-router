package cmd

import (
	"net"
	"net/url"
	"strings"

	"github.com/sjzsdu/free-router/internal/service"
	"github.com/sjzsdu/free-router/internal/tray"
	"github.com/spf13/cobra"
)

func addTrayCommand(root *cobra.Command, opts *options) {
	root.AddCommand(&cobra.Command{
		Use:    "tray",
		Short:  "Run the macOS menu bar controller",
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			manager, err := service.New()
			if err != nil {
				return err
			}
			return tray.Run(cmd.Context(), manager, adminConsoleURL(opts.addr), version)
		},
	})
}

func adminConsoleURL(address string) string {
	address = strings.TrimSpace(address)
	if parsed, err := url.Parse(address); err == nil && parsed.Scheme != "" {
		address = parsed.Host
	}
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		port = "1314"
	}
	return "http://localhost:" + port + "/admin/"
}
