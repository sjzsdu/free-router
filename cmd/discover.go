package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/sjzsdu/free-router/internal/catalog"
	"github.com/sjzsdu/free-router/internal/credentials"
	"github.com/sjzsdu/free-router/internal/provider"
	"github.com/spf13/cobra"
)

type discoveredProvider struct {
	Provider string                     `json:"provider"`
	Models   []provider.DiscoveredModel `json:"models"`
}

type modelProbeFailure struct {
	Model      string `json:"model"`
	Capability string `json:"capability"`
	Error      string `json:"error"`
}

type modelDiscoveryOutput struct {
	Providers     []discoveredProvider       `json:"providers"`
	FetchFailures []catalog.DiscoveryFailure `json:"fetch_failures"`
	ProbeFailures []modelProbeFailure        `json:"probe_failures"`
}

func addDiscoveryCommand(root *cobra.Command, opts *options) {
	root.AddCommand(&cobra.Command{
		Use:    "discover-model-data [provider]",
		Short:  "Discover and verify models for the maintenance Formula",
		Hidden: true,
		Args:   cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := "all"
			if len(args) == 1 {
				target = args[0]
			}
			return discoverModelData(cmd.Context(), *opts, target, cmd.OutOrStdout())
		},
	})
}

func discoverModelData(ctx context.Context, opts options, target string, output io.Writer) error {
	vault := credentials.New(opts.credentials)
	envMap, err := configuredEnvMap(opts)
	if err != nil {
		return err
	}
	registry, err := provider.NewRegistryWithManifest(opts.providers, envMap, opts.freeModels, true, vault.Get)
	if err != nil {
		return err
	}
	if target != "all" {
		if _, ok := registry.Get(target); !ok {
			return fmt.Errorf("provider %q is not configured; configure its API key before targeted discovery", target)
		}
	}
	store := catalog.New(registry, "", catalogHTTPClient())
	models, fetchFailures := store.DiscoverFromProviders(ctx, target)
	byProvider := make(map[string][]provider.DiscoveredModel)
	probeFailures := make([]modelProbeFailure, 0)
	for _, model := range models {
		functions := make([]string, 0, len(model.Functions))
		for _, capability := range model.Functions {
			timeout := 15 * time.Second
			if capability == catalog.FunctionImageGeneration || capability == catalog.FunctionVideoGeneration {
				timeout = 2 * time.Minute
			}
			probeCtx, cancel := context.WithTimeout(ctx, timeout)
			_, probeErr := store.ProbeModel(probeCtx, model.ID, capability)
			cancel()
			if probeErr != nil {
				probeFailures = append(probeFailures, modelProbeFailure{Model: model.ID, Capability: capability, Error: probeErr.Error()})
				continue
			}
			functions = append(functions, capability)
		}
		if len(functions) == 0 {
			continue
		}
		byProvider[model.Provider] = append(byProvider[model.Provider], provider.DiscoveredModel{
			ID: model.UpstreamID, Name: model.Name, Description: model.Description, OwnedBy: model.OwnedBy,
			Type: model.Type, Functions: functions, ContextLength: model.ContextLength,
			MaxOutputTokens: model.MaxOutputTokens, InputModalities: model.InputModalities,
			OutputModalities: model.OutputModalities, SupportedParameters: model.SupportedParameters,
			SupportedEndpoints: model.SupportedEndpoints,
			Pricing:            provider.DiscoveredPricing{Prompt: model.Pricing.Prompt, Completion: model.Pricing.Completion},
		})
	}
	providers := make([]discoveredProvider, 0, len(byProvider))
	for providerID, discovered := range byProvider {
		sort.Slice(discovered, func(i, j int) bool { return discovered[i].ID < discovered[j].ID })
		providers = append(providers, discoveredProvider{Provider: providerID, Models: discovered})
	}
	sort.Slice(providers, func(i, j int) bool { return providers[i].Provider < providers[j].Provider })
	result := modelDiscoveryOutput{Providers: providers, FetchFailures: fetchFailures, ProbeFailures: probeFailures}
	return json.NewEncoder(output).Encode(result)
}
