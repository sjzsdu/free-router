package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sjzsdu/free-router/internal/adapter"
	"github.com/sjzsdu/free-router/internal/catalog"
	"github.com/spf13/cobra"
)

type modelProbeOutcome struct {
	Model    string `json:"model"`
	Provider string `json:"provider"`
	Function string `json:"function"`
	Status   int    `json:"status,omitempty"`
	OK       bool   `json:"ok"`
	Category string `json:"category,omitempty"`
	Error    string `json:"error,omitempty"`
}

type modelValidationResult struct {
	Model      string              `json:"model"`
	Provider   string              `json:"provider"`
	UpstreamID string              `json:"upstream_id"`
	Functions  []modelProbeOutcome `json:"functions"`
}

type validationSummary struct {
	TotalModels   int `json:"total_models"`
	CheckedModels int `json:"checked_models"`
	OKModels      int `json:"ok_models"`
	FailedModels  int `json:"failed_models"`
	TotalProbes   int `json:"total_probes"`
	OKProbes      int `json:"ok_probes"`
	FailedProbes  int `json:"failed_probes"`
}

type validationReport struct {
	CheckedProviders  []string                `json:"checked_providers"`
	SkippedProviders  []string                `json:"skipped_providers"`
	AdapterByProvider map[string]string       `json:"adapter_by_provider,omitempty"`
	Models            []modelValidationResult `json:"models"`
	Summary           validationSummary       `json:"summary"`
}

func addModelValidationCommand(root *cobra.Command, opts *options) {
	var envFile string
	var concurrency int
	var timeout time.Duration
	var exitZero bool
	cmd := &cobra.Command{
		Use:    "validate-models",
		Short:  "Probe every maintained model with a minimal request to verify it actually works",
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(envFile) != "" {
				if _, statErr := os.Stat(envFile); statErr == nil {
					if err := loadEnvFile(envFile); err != nil {
						return err
					}
				} else if !errors.Is(statErr, os.ErrNotExist) {
					return fmt.Errorf("open env file: %w", statErr)
				} else {
					fmt.Fprintf(os.Stderr, "note: env file %q not found; using process environment (set keys in .env, see .env.example)\n", envFile)
				}
			}
			cache := opts.cache
			if strings.TrimSpace(cache) == "" {
				tmp, err := os.CreateTemp("", "free-router-validate-*.json")
				if err != nil {
					return err
				}
				_ = tmp.Close()
				cache = tmp.Name()
				defer os.Remove(cache)
			}
			report, err := validateModels(cmd.Context(), *opts, cache, concurrency, timeout, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			if report.Summary.CheckedModels == 0 && !exitZero {
				return fmt.Errorf("no provider had credentials configured; set the relevant keys in .env (see .env.example) before verifying free models")
			}
			if report.Summary.FailedProbes > 0 && !exitZero {
				return fmt.Errorf("%d model capability probe(s) failed across %d model(s)", report.Summary.FailedProbes, report.Summary.FailedModels)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&envFile, "env-file", ".env", "dotenv file with provider API keys to validate against")
	cmd.Flags().IntVar(&concurrency, "concurrency", 8, "maximum number of concurrent probes")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Second, "per-probe timeout")
	cmd.Flags().BoolVar(&exitZero, "exit-zero", false, "always exit 0, even when probes fail (useful for reporting only)")
	root.AddCommand(cmd)
}

func validateModels(ctx context.Context, opts options, cache string, concurrency int, timeout time.Duration, output io.Writer) (validationReport, error) {
	if concurrency < 1 {
		concurrency = 1
	}
	registry, err := newRegistry(opts)
	if err != nil {
		return validationReport{}, err
	}
	adapters := adapter.NewRegistry()
	store := catalog.New(registry, cache, catalogHTTPClient())
	store.SetProbeBuilder(adapters)
	if err := store.Refresh(ctx); err != nil {
		return validationReport{}, fmt.Errorf("load free model catalog: %w", err)
	}
	models := store.Models()
	sort.Slice(models, func(i, j int) bool {
		if models[i].Provider != models[j].Provider {
			return models[i].Provider < models[j].Provider
		}
		return models[i].ID < models[j].ID
	})

	checked := make(map[string]bool)
	skipped := make(map[string]bool)
	jobs := make([]modelProbeOutcome, 0)
	for _, model := range models {
		spec, ok := registry.Get(model.Provider)
		if !ok || spec.APIKey == "" {
			skipped[model.Provider] = true
			continue
		}
		checked[model.Provider] = true
		if len(model.Functions) == 0 {
			continue
		}
		for _, function := range model.Functions {
			jobs = append(jobs, modelProbeOutcome{Model: model.ID, Provider: model.Provider, Function: function})
		}
	}

	results := make([]modelProbeOutcome, len(jobs))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for idx, job := range jobs {
		wg.Add(1)
		go func(idx int, job modelProbeOutcome) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			probeCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			status, category, probeErr := runModelProbe(probeCtx, store, job.Model, job.Function)
			outcome := job
			outcome.Status = status
			outcome.Category = category
			if probeErr != nil {
				outcome.OK = false
				outcome.Error = probeErr.Error()
			} else {
				outcome.OK = true
			}
			results[idx] = outcome
		}(idx, job)
	}
	wg.Wait()

	byModel := make(map[string]*modelValidationResult, len(models))
	order := make([]string, 0, len(models))
	for _, model := range models {
		if _, ok := checked[model.Provider]; !ok {
			continue
		}
		key := model.ID
		if _, exists := byModel[key]; !exists {
			byModel[key] = &modelValidationResult{Model: model.ID, Provider: model.Provider, UpstreamID: model.UpstreamID}
			order = append(order, key)
		}
	}
	for _, outcome := range results {
		entry := byModel[outcome.Model]
		if entry == nil {
			continue
		}
		entry.Functions = append(entry.Functions, outcome)
	}
	for _, entry := range byModel {
		sort.Slice(entry.Functions, func(i, j int) bool { return entry.Functions[i].Function < entry.Functions[j].Function })
	}

	report := validationReport{
		Models:            make([]modelValidationResult, 0, len(order)),
		AdapterByProvider: make(map[string]string, len(checked)),
	}
	for providerID := range checked {
		if spec, ok := registry.Get(providerID); ok {
			report.AdapterByProvider[providerID] = adapters.Resolve(spec).Name()
		}
	}
	for _, key := range order {
		report.Models = append(report.Models, *byModel[key])
	}
	report.CheckedProviders = sortedKeys(checked)
	report.SkippedProviders = sortedKeys(skipped)

	summary := validationSummary{TotalModels: len(models), CheckedModels: len(order)}
	for _, entry := range report.Models {
		modelOK := true
		for _, fn := range entry.Functions {
			summary.TotalProbes++
			if fn.OK {
				summary.OKProbes++
			} else {
				summary.FailedProbes++
				modelOK = false
			}
		}
		if modelOK {
			summary.OKModels++
		} else {
			summary.FailedModels++
		}
	}
	report.Summary = summary

	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		return report, err
	}

	fmt.Fprintf(os.Stderr, "validated %d models across %d provider(s): %d/%d capabilities ok",
		summary.CheckedModels, len(report.CheckedProviders), summary.OKProbes, summary.TotalProbes)
	if len(report.SkippedProviders) > 0 {
		fmt.Fprintf(os.Stderr, "; skipped %d provider(s) without credentials: %s",
			len(report.SkippedProviders), strings.Join(report.SkippedProviders, ", "))
	}
	fmt.Fprintln(os.Stderr)
	if summary.FailedModels > 0 {
		fmt.Fprintf(os.Stderr, "FAILED models (%d):\n", summary.FailedModels)
		for _, entry := range report.Models {
			for _, fn := range entry.Functions {
				if !fn.OK {
					fmt.Fprintf(os.Stderr, "  - %s [%s/%s] %s\n", entry.Model, entry.Provider, fn.Function, fn.Error)
				}
			}
		}
	}
	return report, nil
}

func runModelProbe(ctx context.Context, store *catalog.Store, modelID, function string) (int, string, error) {
	result, err := store.ProbeModel(ctx, modelID, function)
	if err == nil {
		return result.Status, "", nil
	}
	var probeErr *catalog.ModelProbeError
	if errors.As(err, &probeErr) {
		return probeErr.Status, httpErrorCategory(probeErr.Status), err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return 0, "timeout", err
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return 0, "network", err
	}
	return 0, "unknown", err
}

func httpErrorCategory(status int) string {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return "authentication"
	case http.StatusPaymentRequired:
		return "quota"
	case http.StatusTooManyRequests:
		return "rate-limit"
	case http.StatusNotFound, http.StatusGone:
		return "unavailable"
	}
	if status >= http.StatusInternalServerError {
		return "upstream"
	}
	if status >= http.StatusBadRequest {
		return "http"
	}
	return "unknown"
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// loadEnvFile parses a simple KEY=VALUE dotenv file and exports each entry into
// the process environment so the provider registry can resolve credentials from
// it. Existing environment variables are not overridden.
func loadEnvFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open env file: %w", err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := parseEnvLine(line)
		if !ok {
			return fmt.Errorf("%s:%d: invalid line %q", path, lineNo, line)
		}
		if os.Getenv(key) == "" {
			if err := os.Setenv(key, value); err != nil {
				return fmt.Errorf("set env %s: %w", key, err)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read env file: %w", err)
	}
	return nil
}

func parseEnvLine(line string) (string, string, bool) {
	idx := strings.Index(line, "=")
	if idx <= 0 {
		return "", "", false
	}
	key := strings.TrimSpace(line[:idx])
	value := strings.TrimSpace(line[idx+1:])
	if strings.HasPrefix(value, "\"") && strings.HasSuffix(value, "\"") && len(value) >= 2 {
		value = value[1 : len(value)-1]
	} else if strings.HasPrefix(value, "'") && strings.HasSuffix(value, "'") && len(value) >= 2 {
		value = value[1 : len(value)-1]
	}
	if key == "" {
		return "", "", false
	}
	return key, value, true
}
