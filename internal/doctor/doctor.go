// Package doctor collects a read-only diagnosis of a running (or broken)
// free-router installation.
//
// The pieces needed to answer "why can't I use it today" already exist —
// health state, usage statistics, the gateway's recent failures, the daemon
// log — but they live in five places and each speaks its own vocabulary. This
// package joins them into one list of findings with a concrete next step, so a
// user (or an agent reading CLI output) does not have to know the layout.
package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/sjzsdu/free-router/internal/credentials"
	"github.com/sjzsdu/free-router/internal/provider"
	"github.com/sjzsdu/free-router/internal/routing"
)

// Status is one finding's severity. Fail marks a problem that stops free-router
// from serving, warn something the user should know about, skip a check that
// could not run (for example because the service is down).
type Status string

const (
	StatusOK   Status = "ok"
	StatusWarn Status = "warn"
	StatusFail Status = "fail"
	StatusSkip Status = "skip"
)

// Check is one finding.
type Check struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail"`
	Hint   string `json:"hint,omitempty"`
}

// Report is the full diagnosis, ordered as the checks ran.
type Report struct {
	Checks []Check `json:"checks"`
}

// Failed reports whether any check failed (which is what should decide the
// command's exit status).
func (r Report) Failed() bool {
	for _, check := range r.Checks {
		if check.Status == StatusFail {
			return true
		}
	}
	return false
}

// Options are the local paths and endpoint to inspect. They mirror the serve
// flags so doctor can be pointed at the same installation.
type Options struct {
	Addr            string
	ConfigPath      string
	FreeModels      string
	CredentialsPath string
	HealthPath      string
	StatisticsPath  string
	LogPath         string
	// AdminToken authenticates the read-only admin API calls when the service
	// requires one. An empty token simply means those checks are skipped.
	AdminToken string
	Client     *http.Client
}

const (
	// A daemon log that never rotates is the usual reason a long-lived
	// installation fills a disk; the threshold is deliberately generous.
	logWarningBytes   = 50 << 20
	stateWarningBytes = 20 << 20
)

// Run performs every check. Checks never abort each other: a broken
// configuration should still report the state of the log file.
func Run(ctx context.Context, opts Options) Report {
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Second}
	}
	report := Report{}
	report.Checks = append(report.Checks, checkConfig(opts))
	providers := checkProviders(opts)
	report.Checks = append(report.Checks, providers)
	report.Checks = append(report.Checks, checkStorage(opts))
	health, reachable := checkService(ctx, client, opts)
	report.Checks = append(report.Checks, health)
	report.Checks = append(report.Checks, checkFailures(ctx, client, opts, reachable))
	report.Checks = append(report.Checks, checkLog(opts))
	return report
}

func checkConfig(opts Options) Check {
	routes, err := routing.New(opts.ConfigPath)
	if err != nil {
		return Check{
			Name: "route configuration", Status: StatusFail,
			Detail: fmt.Sprintf("%s could not be loaded: %v", opts.ConfigPath, err),
			Hint:   "fix the JSON at that path, or run `free-router onboard` to regenerate a documented configuration",
		}
	}
	config := routes.Config()
	empty := 0
	for _, route := range config.Routes {
		if len(route.Models) == 0 {
			empty++
		}
	}
	detail := fmt.Sprintf("%s is valid (version %d, revision %d, %d routes, %d with an explicit fallback list)",
		opts.ConfigPath, config.Version, config.Revision, len(config.Routes), len(config.Routes)-empty)
	hint := ""
	if empty == len(config.Routes) {
		detail += "; every route is fully automatic"
		hint = "automatic routes are fine, but pinning a few models per route makes fallback order predictable"
	}
	return Check{Name: "route configuration", Status: StatusOK, Detail: detail, Hint: hint}
}

func checkProviders(opts Options) Check {
	// The route configuration is what maps environment variables to provider
	// keys, so a broken config must not make the provider check lie.
	envMap := provider.DefaultEnvMap()
	if routes, err := routing.New(opts.ConfigPath); err == nil {
		envMap = provider.EnvMap(routes.Config().ProviderEnv)
	}
	vault := credentials.New(opts.CredentialsPath)
	statuses := provider.BuiltinStatusWithManifest(envMap, opts.FreeModels, vault.Get)
	configured := make([]string, 0, len(statuses))
	missing := make([]string, 0, len(statuses))
	for _, status := range statuses {
		id, _ := status["id"].(string)
		flag, _ := status["configured"].(bool)
		if flag {
			configured = append(configured, id)
			continue
		}
		missing = append(missing, id)
	}
	if len(configured) == 0 {
		return Check{
			Name: "provider credentials", Status: StatusFail,
			Detail: fmt.Sprintf("none of the %d built-in providers is configured", len(statuses)),
			Hint:   "run `free-router setup <provider>` (see `free-router providers` for the list) or export a provider API key",
		}
	}
	models := 0
	for _, status := range statuses {
		if count, ok := status["formula_model_count"].(int); ok {
			models += count
		}
	}
	detail := fmt.Sprintf("%d of %d providers configured (%d maintained models): %s",
		len(configured), len(statuses), models, strings.Join(configured, ", "))
	hint := ""
	if len(missing) > 0 {
		hint = fmt.Sprintf("not configured yet: %s — add one to widen the fallback pool", strings.Join(missing, ", "))
	}
	return Check{Name: "provider credentials", Status: StatusOK, Detail: detail, Hint: hint}
}

func checkStorage(opts Options) Check {
	paths := []struct {
		name      string
		path      string
		warnBytes int64
	}{
		{name: "health", path: opts.HealthPath, warnBytes: stateWarningBytes},
		{name: "statistics", path: opts.StatisticsPath, warnBytes: stateWarningBytes},
	}
	details := make([]string, 0, len(paths))
	status := StatusOK
	hint := ""
	for _, item := range paths {
		info, err := os.Stat(item.path)
		if err != nil {
			details = append(details, fmt.Sprintf("%s: not created yet (%s)", item.name, item.path))
			continue
		}
		details = append(details, fmt.Sprintf("%s: %s, %s, updated %s",
			item.name, humanBytes(info.Size()), item.path, info.ModTime().Format(time.RFC3339)))
		if info.Size() > item.warnBytes {
			status = StatusWarn
			hint = "state files are unexpectedly large; they can be deleted while the service is stopped (history is lost, routing is unaffected)"
		}
	}
	return Check{Name: "local state", Status: status, Detail: strings.Join(details, "; "), Hint: hint}
}

// healthzResponse mirrors the subset of /healthz that doctor reads.
type healthzResponse struct {
	Status       string `json:"status"`
	FreeModels   int    `json:"free_models"`
	Providers    int    `json:"providers"`
	CatalogCount int    `json:"catalog_count"`
}

func checkService(ctx context.Context, client *http.Client, opts Options) (Check, bool) {
	endpoint := serviceURL(opts.Addr, "/healthz")
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Check{Name: "running service", Status: StatusSkip, Detail: fmt.Sprintf("could not build request for %s", endpoint)}, false
	}
	response, err := client.Do(request)
	if err != nil {
		return Check{
			Name: "running service", Status: StatusWarn,
			Detail: fmt.Sprintf("no answer from %s: %v", endpoint, err),
			Hint:   "start it with `free-router serve` or `free-router daemon start`; `free-router daemon status` shows whether a daemon is installed",
		}, false
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if response.StatusCode != http.StatusOK {
		return Check{
			Name: "running service", Status: StatusWarn,
			Detail: fmt.Sprintf("%s answered %d: %s", endpoint, response.StatusCode, strings.TrimSpace(string(body))),
			Hint:   "inspect the process log for a startup failure",
		}, false
	}
	var health healthzResponse
	if err := json.Unmarshal(body, &health); err != nil {
		return Check{Name: "running service", Status: StatusWarn, Detail: fmt.Sprintf("%s returned unreadable JSON", endpoint)}, true
	}
	return Check{
		Name: "running service", Status: StatusOK,
		Detail: fmt.Sprintf("%s is up: %d providers, %d maintained models, %d catalog entries",
			endpoint, health.Providers, health.FreeModels, health.CatalogCount),
	}, true
}

// failureRecord mirrors the fields of the gateway's recent-failure log that
// doctor surfaces.
type failureRecord struct {
	At        time.Time `json:"at"`
	RequestID string    `json:"request_id"`
	Route     string    `json:"route"`
	Status    int       `json:"status"`
	Code      string    `json:"code"`
	Message   string    `json:"message"`
}

func checkFailures(ctx context.Context, client *http.Client, opts Options, reachable bool) Check {
	if !reachable {
		return Check{
			Name: "recent failures", Status: StatusSkip,
			Detail: "skipped: the service is not answering, so its in-memory failure log is unavailable",
		}
	}
	endpoint := serviceURL(opts.Addr, "/admin/api/failures")
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Check{Name: "recent failures", Status: StatusSkip, Detail: fmt.Sprintf("could not build request for %s", endpoint)}
	}
	if opts.AdminToken != "" {
		request.Header.Set("Authorization", "Bearer "+opts.AdminToken)
	}
	response, err := client.Do(request)
	if err != nil {
		return Check{Name: "recent failures", Status: StatusWarn, Detail: fmt.Sprintf("%s: %v", endpoint, err)}
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized {
		return Check{
			Name: "recent failures", Status: StatusSkip,
			Detail: "skipped: the admin API requires a token and this process has none",
			Hint:   "export FREE_ROUTER_ADMIN_TOKEN so doctor can read /admin/api/failures",
		}
	}
	if response.StatusCode != http.StatusOK {
		return Check{Name: "recent failures", Status: StatusSkip, Detail: fmt.Sprintf("%s answered %d", endpoint, response.StatusCode)}
	}
	var payload struct {
		Failures []failureRecord `json:"failures"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&payload); err != nil {
		return Check{Name: "recent failures", Status: StatusWarn, Detail: "failure log could not be decoded"}
	}
	if len(payload.Failures) == 0 {
		return Check{Name: "recent failures", Status: StatusOK, Detail: "no request has exhausted the pool since startup"}
	}
	latest := payload.Failures[0]
	return Check{
		Name: "recent failures", Status: StatusWarn,
		Detail: fmt.Sprintf("%d recent pool-wide failures; latest %s on route %q (request %s): %s",
			len(payload.Failures), latest.Code, latest.Route, latest.RequestID, latest.Message),
		Hint: "run with `--log-level debug` and compare the failing candidates, or reset the affected models on the admin health page",
	}
}

func checkLog(opts Options) Check {
	if strings.TrimSpace(opts.LogPath) == "" {
		return Check{Name: "service log", Status: StatusSkip, Detail: "no log path could be determined for this platform"}
	}
	info, err := os.Stat(opts.LogPath)
	if err != nil {
		return Check{
			Name: "service log", Status: StatusOK,
			Detail: fmt.Sprintf("%s does not exist yet (the daemon has not written to it)", opts.LogPath),
			Hint:   "foreground `free-router serve` logs to stderr instead",
		}
	}
	detail := fmt.Sprintf("%s, %s, updated %s", opts.LogPath, humanBytes(info.Size()), info.ModTime().Format(time.RFC3339))
	if info.Size() > logWarningBytes {
		return Check{
			Name: "service log", Status: StatusWarn, Detail: detail,
			Hint: "the daemon log never rotates; stop the daemon, move or delete the file, then start it again",
		}
	}
	return Check{Name: "service log", Status: StatusOK, Detail: detail}
}

func serviceURL(addr, path string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		addr = "127.0.0.1:1314"
	}
	if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
		return strings.TrimRight(addr, "/") + path
	}
	return "http://" + addr + path
}

func humanBytes(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	divisor, exponent := int64(unit), 0
	for value := size / unit; value >= unit; value /= unit {
		divisor *= unit
		exponent++
	}
	return fmt.Sprintf("%.1f %ciB", float64(size)/float64(divisor), "KMGTPE"[exponent])
}
