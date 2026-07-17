package cmd

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/sjzsdu/free-router/internal/credentials"
	"github.com/sjzsdu/free-router/internal/provider"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func addAuthCommands(root *cobra.Command, opts *options) {
	setup := &cobra.Command{
		Use:   "setup [provider]",
		Short: "Save a provider API key once (macOS Keychain when available)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			providerID := ""
			if len(args) == 1 {
				providerID = args[0]
			}
			return addCredential(cmd, opts, providerID)
		},
	}

	auth := &cobra.Command{Use: "auth", Short: "Manage saved provider API keys"}
	auth.AddCommand(&cobra.Command{
		Use:   "add [provider]",
		Short: "Add or replace a saved API key",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			providerID := ""
			if len(args) == 1 {
				providerID = args[0]
			}
			return addCredential(cmd, opts, providerID)
		},
	})
	auth.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List saved keys without displaying their values",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			vault := credentials.New(opts.credentials)
			entries, err := vault.List()
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(entries)
		},
	})
	auth.AddCommand(&cobra.Command{
		Use:   "remove <provider>",
		Short: "Remove a saved API key",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			vault := credentials.New(opts.credentials)
			if err := vault.Delete(args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed credential for %s\n", args[0])
			return nil
		},
	})

	root.AddCommand(setup, auth)
}

func addCredential(cmd *cobra.Command, opts *options, providerID string) error {
	reader := bufio.NewReader(cmd.InOrStdin())
	if strings.TrimSpace(providerID) == "" {
		fmt.Fprintln(cmd.ErrOrStderr(), "Available built-in providers:")
		envMap, err := configuredEnvMap(*opts)
		if err != nil {
			return err
		}
		for _, status := range provider.BuiltinStatusWithEnv(envMap) {
			fmt.Fprintf(cmd.ErrOrStderr(), "  %s\n", status["id"])
		}
		fmt.Fprint(cmd.ErrOrStderr(), "Provider ID: ")
		value, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		providerID = strings.TrimSpace(value)
	}
	if providerID == "" {
		return errors.New("provider must not be empty")
	}

	fmt.Fprint(cmd.ErrOrStderr(), "API key: ")
	secret, err := readSecret(cmd.InOrStdin(), reader)
	fmt.Fprintln(cmd.ErrOrStderr())
	if err != nil {
		return err
	}
	if secret == "" {
		return errors.New("API key must not be empty")
	}

	vault := credentials.New(opts.credentials)
	backend, err := vault.Set(providerID, secret)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Saved credential for %s using %s\n", providerID, backend)
	if backend == "file" {
		fmt.Fprintf(cmd.OutOrStdout(), "Credential file: %s (mode 0600)\n", vault.Path())
	}
	return nil
}

func readSecret(input io.Reader, reader *bufio.Reader) (string, error) {
	if file, ok := input.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		secret, err := term.ReadPassword(int(file.Fd()))
		return strings.TrimSpace(string(secret)), err
	}
	secret, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(secret), nil
}
