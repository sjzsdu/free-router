package cmd

import (
	"context"
	"encoding/json"
	"io"
	"sort"

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

type discoverySkip struct {
	Provider    string   `json:"provider"`
	Reason      string   `json:"reason"`
	RequiredEnv []string `json:"required_env,omitempty"`
}

type modelDiscoveryOutput struct {
	Providers     []discoveredProvider       `json:"providers"`
	Checked       []string                   `json:"checked_providers"`
	Available     map[string]bool            `json:"available"`
	FetchFailures []catalog.DiscoveryFailure `json:"fetch_failures"`
	ProbeFailures []modelProbeFailure        `json:"probe_failures"`
	Skipped       []discoverySkip            `json:"skipped_providers"`
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
	registry, err := provider.NewRegistryWithManifest(opts.providers, envMap, opts.freeModels, vault.Get)
	if err != nil {
		return err
	}
	store := catalog.New(registry, "", catalogHTTPClient())
	specs := make([]provider.Spec, 0)
	skipped := make([]discoverySkip, 0)
	for _, catalogSpec := range registry.CatalogAll() {
		if target != "all" && catalogSpec.ID != target {
			continue
		}
		if catalogSpec.ModelDiscovery == "agent" {
			skipped = append(skipped, discoverySkip{Provider: catalogSpec.ID, Reason: "agent-maintained"})
			continue
		}
		if configured, ok := registry.Get(catalogSpec.ID); ok {
			catalogSpec = configured
		} else if !catalogSpec.NoAuth {
			required := append([]string(nil), catalogSpec.RequiredEnvs...)
			if catalogSpec.APIKeyEnv != "" {
				required = append([]string{catalogSpec.APIKeyEnv}, required...)
			}
			skipped = append(skipped, discoverySkip{Provider: catalogSpec.ID, Reason: "missing-credentials", RequiredEnv: required})
			continue
		}
		specs = append(specs, catalogSpec)
	}
	sort.Slice(skipped, func(i, j int) bool { return skipped[i].Provider < skipped[j].Provider })
	models, fetchFailures := store.DiscoverFromSpecs(ctx, specs)
	failedProviders := make(map[string]bool, len(fetchFailures))
	for _, failure := range fetchFailures {
		failedProviders[failure.Provider] = true
	}
	checked := make([]string, 0)
	available := make(map[string]bool, len(specs))
	for _, spec := range specs {
		available[spec.ID] = !failedProviders[spec.ID]
		if available[spec.ID] {
			checked = append(checked, spec.ID)
		}
	}
	byProvider := make(map[string][]provider.DiscoveredModel)
	for _, model := range models {
		byProvider[model.Provider] = append(byProvider[model.Provider], provider.DiscoveredModel{
			ID: model.UpstreamID, Name: model.Name, Description: model.Description, OwnedBy: model.OwnedBy,
			Type: model.Type, Functions: model.Functions, ContextLength: model.ContextLength,
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
	for _, item := range skipped {
		available[item.Provider] = false
	}
	result := modelDiscoveryOutput{Providers: providers, Checked: checked, Available: available, FetchFailures: fetchFailures, ProbeFailures: []modelProbeFailure{}, Skipped: skipped}
	return json.NewEncoder(output).Encode(result)
}
