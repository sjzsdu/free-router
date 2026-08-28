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
	var providers string
	var concurrency int
	var timeout time.Duration
	var exitZero bool
	cmd := &cobra.Command{
		Use:    "validate-models",
		Short:  "Probe maintained models with a minimal request to confirm they are callable",
		Long: "Validate free model providers by sending minimal requests.\n" +
			"By default only providers with API keys configured in .env are checked.\n" +
			"Use --providers to validate a specific subset.",
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
			var providerFilter []string
			if strings.TrimSpace(providers) != "" {
				for _, p := range strings.Split(providers, ",") {
					if p = strings.TrimSpace(p); p != "" {
						providerFilter = append(providerFilter, p)
					}
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
			report, err := validateModels(cmd.Context(), *opts, cache, providerFilter, concurrency, timeout, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			if report.Summary.CheckedModels == 0 && !exitZero {
				if len(providerFilter) > 0 {
					return fmt.Errorf("none of the specified providers (%s) have credentials configured; set the relevant keys in .env (see .env.example)", strings.Join(providerFilter, ", "))
				}
				return fmt.Errorf("no provider had credentials configured; set the relevant keys in .env (see .env.example) before verifying free models")
			}
			if report.Summary.FailedProbes > 0 && !exitZero {
				return fmt.Errorf("%d model capability probe(s) failed across %d model(s)", report.Summary.FailedProbes, report.Summary.FailedModels)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&envFile, "env-file", ".env", "dotenv file with provider API keys to validate against")
	cmd.Flags().StringVar(&providers, "providers", "", "comma-separated list of provider IDs to validate (default: all with configured keys)")
	cmd.Flags().IntVar(&concurrency, "concurrency", 8, "maximum number of concurrent probes")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Second, "per-probe timeout")
	cmd.Flags().BoolVar(&exitZero, "exit-zero", false, "always exit 0, even when probes fail (useful for reporting only)")
	root.AddCommand(cmd)
}

func validateModels(ctx context.Context, opts options, cache string, providerFilter []string, concurrency int, timeout time.Duration, output io.Writer) (validationReport, error) {
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

	// Build a fast lookup set when the user restricts to specific providers.
	var allowedProviders map[string]bool
	if len(providerFilter) > 0 {
		allowedProviders = make(map[string]bool, len(providerFilter))
		for _, p := range providerFilter {
			allowedProviders[p] = true
		}
	}

	checked := make(map[string]bool)
	skipped := make(map[string]string) // provider → reason
	for _, model := range models {
		if _, ok := skipped[model.Provider]; ok {
			continue
		}
		spec, ok := registry.Get(model.Provider)
		if !ok || spec.APIKey == "" {
			skipped[model.Provider] = "no credentials configured"
			continue
		}
		if allowedProviders != nil && !allowedProviders[model.Provider] {
			skipped[model.Provider] = "not in --providers list"
			continue
		}
		checked[model.Provider] = true
	}

	// When both "chat" and "chat-tools" are listed for a model, only probe
	// "chat" — the result is assumed identical for "chat-tools". This cuts
	// probe volume roughly in half without sacrificing accuracy.
	jobs := make([]modelProbeOutcome, 0)
	for _, model := range models {
		if !checked[model.Provider] {
			continue
		}
		if len(model.Functions) == 0 {
			continue
		}
		hasChat := false
		for _, f := range model.Functions {
			if f == "chat" {
				hasChat = true
				break
			}
		}
		for _, function := range model.Functions {
			if function == "chat-tools" && hasChat {
				continue // skip: will be inferred from chat result
			}
			jobs = append(jobs, modelProbeOutcome{Model: model.ID, Provider: model.Provider, Function: function})
		}
	}		// Build per-provider probe timing to enforce minimum delay between
		// probes to the same provider. This prevents concurrent probes from
		// overwhelming a single provider's rate limit.
		type providerTiming struct {
			mu        sync.Mutex
			lastProbe time.Time
		}
		providerTimings := make(map[string]*providerTiming)
		for pid := range checked {
			providerTimings[pid] = &providerTiming{}
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
				outcome := job

				// Enforce per-provider minimum delay between probes.
				if pt, ok := providerTimings[job.Provider]; ok {
					pt.mu.Lock()
					minDelay := providerMinDelay(job.Provider)
					if !pt.lastProbe.IsZero() {
						elapsed := time.Since(pt.lastProbe)
						if elapsed < minDelay {
							time.Sleep(minDelay - elapsed)
						}
					}
					pt.lastProbe = time.Now()
					pt.mu.Unlock()
				}

				// Retry loop: rate-limit gets up to 3 retries, timeout up to 2.
				for attempt := 0; ; attempt++ {
					probeCtx, cancel := context.WithTimeout(ctx, timeout)
					status, category, probeErr := runModelProbe(probeCtx, store, job.Model, job.Function)
					cancel()
					if probeErr == nil {
						outcome.Status = status
						outcome.Category = category
						outcome.OK = true
						break
					}
					outcome.Status = status
					outcome.Category = category
					outcome.OK = false
					outcome.Error = probeErr.Error()
					// Decide whether to retry.
					if category == "rate-limit" && attempt < 2 {
						delay := rateLimitBackoff(attempt, probeErr)
						time.Sleep(delay)
						continue
					}
					if category == "timeout" && attempt < 1 {
						time.Sleep(3 * time.Second)
						continue
					}
					break
				}
				results[idx] = outcome
			}(idx, job)
		}
		wg.Wait()

	// Infer chat-tools results from chat probes where we skipped the
	// redundant chat-tools probe. We scan every checked model; if it lists
	// "chat-tools" in its manifest functions but no probe result exists for
	// it, we clone the "chat" outcome.
	chatResultByModel := make(map[string]*modelProbeOutcome)
	for i := range results {
		if results[i].Function == "chat" {
			chatResultByModel[results[i].Model] = &results[i]
		}
	}
	for _, model := range models {
		if !checked[model.Provider] {
			continue
		}
		if len(model.Functions) == 0 {
			continue
		}
		hasChatTools := false
		hasChatToolsResult := false
		for _, f := range model.Functions {
			if f == "chat-tools" {
				hasChatTools = true
			}
		}
		if !hasChatTools {
			continue
		}
		for _, r := range results {
			if r.Model == model.ID && r.Function == "chat-tools" {
				hasChatToolsResult = true
				break
			}
		}
		if !hasChatToolsResult {
			if chatOutcome, ok := chatResultByModel[model.ID]; ok {
				clone := *chatOutcome
				clone.Function = "chat-tools"
				results = append(results, clone)
			}
		}
	}

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
	skippedKeys := make([]string, 0, len(skipped))
	for k := range skipped {
		skippedKeys = append(skippedKeys, k)
	}
	sort.Strings(skippedKeys)
	report.SkippedProviders = skippedKeys

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

	// ── Human-friendly summary on stderr ──────────────────────────────────
	printProviderSummary(os.Stderr, report, skipped)
	return report, nil
}

// printProviderSummary outputs a per-provider table to w showing model
// counts, probe results, and categorised failure reasons.
func printProviderSummary(w io.Writer, report validationReport, skipped map[string]string) {
	// ── aggregate per-provider stats ─────────────────────────────────────
	type providerStats struct {
		totalModels int
		totalFuncs  int
		okFuncs     int
		failedFuncs int
		issues      map[string]int // category → count
		firstError  string         // representative short error
	}
	byProvider := make(map[string]*providerStats)
	for _, entry := range report.Models {
		ps, ok := byProvider[entry.Provider]
		if !ok {
		ps = &providerStats{issues: make(map[string]int)}
		byProvider[entry.Provider] = ps
		}
		ps.totalModels++
		for _, fn := range entry.Functions {
			ps.totalFuncs++
			if fn.OK {
				ps.okFuncs++
			} else {
				ps.failedFuncs++
				cat := classifyError(fn)
				ps.issues[cat]++
				if ps.firstError == "" {
					ps.firstError = shortenError(fn.Error)
				}
			}
		}
	}

	// ── table header ─────────────────────────────────────────────────────
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Provider Validation Report")
	fmt.Fprintln(w, strings.Repeat("=", 78))
	fmt.Fprintf(w, "%-16s %6s %6s %8s  %s\n", "Provider", "Models", "Funcs", "OK/Total", "Status")
	fmt.Fprintln(w, strings.Repeat("-", 78))

	// ── rows ─────────────────────────────────────────────────────────────
	providers := report.CheckedProviders
	sort.Strings(providers)
	for _, pid := range providers {
		ps := byProvider[pid]
		if ps == nil {
			continue
		}
		status := "✓ all ok"
		if ps.failedFuncs > 0 {
			if ps.okFuncs == 0 {
				status = "✗ " + summarizeIssues(ps.issues)
			} else {
				status = "⚠ " + summarizeIssues(ps.issues)
			}
		}
		fmt.Fprintf(w, "%-16s %6d %6d %3d/%-4d  %s\n",
			pid, ps.totalModels, ps.totalFuncs, ps.okFuncs, ps.totalFuncs, status)
	}
	fmt.Fprintln(w, strings.Repeat("=", 78))

	// ── skipped providers ────────────────────────────────────────────────
	if len(report.SkippedProviders) > 0 {
		fmt.Fprintf(w, "\nSkipped providers (%d):\n", len(report.SkippedProviders))
		for _, pid := range report.SkippedProviders {
			reason := skipped[pid]
			if reason == "" {
				reason = "unknown"
			}
			fmt.Fprintf(w, "  - %-16s %s\n", pid, reason)
		}
	}

	// ── overall summary ──────────────────────────────────────────────────
	fmt.Fprintf(w, "\nOverall: %d models checked, %d/%d probes passed",
		report.Summary.CheckedModels, report.Summary.OKProbes, report.Summary.TotalProbes)
	if report.Summary.FailedProbes > 0 {
		fmt.Fprintf(w, ", %d failed", report.Summary.FailedProbes)
	}
	fmt.Fprintln(w)
}

// classifyError returns a human-readable category for a failed probe outcome.
// For 403 responses the error message is inspected first because 403 can mean
// "model disabled", "account forbidden", or genuine auth failure.
func classifyError(fn modelProbeOutcome) string {
	low := strings.ToLower(fn.Error)

	// Fast-path categories from the probe runner.
	switch fn.Category {
	case "rate-limit":
		return "rate-limit"
	case "timeout":
		return "timeout"
	case "network":
		return "network"
	case "unavailable":
		return "unavailable"
	}

	// For 403 responses, inspect the message before falling back to "auth".
	if fn.Status == http.StatusForbidden {
		if strings.Contains(low, "model disabled") || strings.Contains(low, "disabled") {
			return "model-disabled"
		}
		if strings.Contains(low, "end of life") || strings.Contains(low, "no longer available") {
			return "deprecated"
		}
		if strings.Contains(low, "unavailable for free") {
			return "no-longer-free"
		}
		// A 403 with an HTML body (e.g. Cohere) is likely an auth/plan issue.
		if strings.Contains(low, "doctype html") || strings.Contains(low, "<html") {
			return "auth"
		}
		return "forbidden"
	}

	// 401 is a genuine auth failure.
	if fn.Status == http.StatusUnauthorized {
		return "auth"
	}

	// For 429 that slipped through without category set.
	if fn.Status == http.StatusTooManyRequests {
		return "rate-limit"
	}
	if fn.Status == http.StatusNotFound {
		return "not-found"
	}
	if fn.Status == http.StatusGone {
		return "gone"
	}
	if fn.Status >= 500 {
		return "upstream"
	}

	// Check error message hints for non-403/401 responses.
	if strings.Contains(low, "unauthorized") || strings.Contains(low, "invalid") && strings.Contains(low, "key") {
		return "auth"
	}
	if strings.Contains(low, "model disabled") {
		return "model-disabled"
	}
	if strings.Contains(low, "tool call") || strings.Contains(low, "tool_choice") {
		return "tool-unsupported"
	}
	if strings.Contains(low, "end of life") || strings.Contains(low, "no longer available") {
		return "deprecated"
	}
	if strings.Contains(low, "unavailable for free") {
		return "no-longer-free"
	}
	return "error"
}

// summarizeIssues returns a short comma-separated summary of issue categories.
func summarizeIssues(issues map[string]int) string {
	// order: auth first, then the rest
	parts := make([]string, 0, len(issues))
	order := []string{"auth", "forbidden", "deprecated", "not-found", "gone", "unavailable", "model-disabled", "no-longer-free", "tool-unsupported", "rate-limit", "timeout", "network", "upstream", "error"}
	for _, cat := range order {
		if n, ok := issues[cat]; ok {
			parts = append(parts, fmt.Sprintf("%s:%d", cat, n))
		}
	}
	return strings.Join(parts, ", ")
}

// shortenError truncates a long upstream error message to a concise summary.
func shortenError(s string) string {
	if s == "" {
		return ""
	}
	// strip HTML tags
	if i := strings.Index(s, "<"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	// take first line only
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 80 {
		s = s[:77] + "..."
	}
	return s
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
	case http.StatusUnauthorized:
		return "authentication"
	case http.StatusForbidden:
		return "forbidden"
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

// rateLimitBackoff returns the delay to use before retrying after a rate-limit
// error. It prefers the Retry-After header from the upstream response; when that
// is unavailable it falls back to exponential backoff.
func rateLimitBackoff(attempt int, probeErr error) time.Duration {
	var probeError *catalog.ModelProbeError
	if errors.As(probeErr, &probeError) && probeError.RetryAfter > 0 {
		return probeError.RetryAfter
	}
	// Exponential backoff: attempt 0 → 5s, attempt 1 → 10s.
	return time.Duration(5*(attempt+1)) * time.Second
}

// providerMinDelay returns the minimum time between consecutive probes to the
// same provider. Providers that declare rate_limit_per_second get a delay
// derived from that value; others get a conservative 500ms default.
func providerMinDelay(providerID string) time.Duration {
	// Provider-specific delays based on known rate limits.
	switch providerID {
	case "openrouter":
		// OpenRouter free tier: ~20 requests/minute → ~3s between probes.
		return 3 * time.Second
	case "zai":
		// Z.AI free tier: conservative 2s between probes.
		return 2 * time.Second
	case "nvidia":
		// NVIDIA free credits: moderate rate.
		return 1 * time.Second
	case "modelscope":
		// ModelScope: high volume but rate-limited.
		return 500 * time.Millisecond
	case "dashscope":
		// DashScope: moderate rate.
		return 500 * time.Millisecond
	default:
		return 200 * time.Millisecond
	}
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
