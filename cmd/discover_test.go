package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/sjzsdu/free-router/internal/provider"
)

func TestDiscoverModelDataUsesOfficialCatalogWithoutInferenceProbes(t *testing.T) {
	for _, key := range provider.SupportedKeyEnvs() {
		t.Setenv(key, "")
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"working","supported_parameters":["tools"]},{"id":"broken"}]}`))
		case "/chat/completions":
			t.Fatal("Formula inventory discovery must not call inference endpoints")
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	opts := defaultOptions()
	opts.providers = `[{"id":"test","base_url":"` + upstream.URL + `","no_auth":true},{"id":"must-not-be-called","base_url":"http://127.0.0.1:1","no_auth":true}]`
	dir := t.TempDir()
	opts.config = filepath.Join(dir, "config.json")
	opts.credentials = filepath.Join(dir, "credentials.json")
	opts.freeModels = filepath.Join(dir, "free-models.json")
	manifest := `{"schema_version":2,"generated_at":"test","providers":{"test":{"source_urls":["https://example.com/models"],"models":[]}}}`
	if err := os.WriteFile(opts.freeModels, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := discoverModelData(context.Background(), opts, "test", &output); err != nil {
		t.Fatal(err)
	}
	var result modelDiscoveryOutput
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Providers) != 1 || len(result.Providers[0].Models) != 2 {
		t.Fatalf("result=%#v", result)
	}
	if len(result.FetchFailures) != 0 {
		t.Fatalf("targeted discovery contacted another provider: %#v", result.FetchFailures)
	}
	if len(result.Checked) != 1 || result.Checked[0] != "test" {
		t.Fatalf("checked providers=%#v", result.Checked)
	}
	if !result.Available["test"] {
		t.Fatalf("available=%#v", result.Available)
	}
	if len(result.ProbeFailures) != 0 {
		t.Fatalf("probe failures=%#v", result.ProbeFailures)
	}
}

func TestDiscoverModelDataDoesNotMarkFetchFailureAsChecked(t *testing.T) {
	for _, key := range provider.SupportedKeyEnvs() {
		t.Setenv(key, "")
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
	}))
	defer upstream.Close()

	opts := defaultOptions()
	opts.providers = `[{"id":"test","base_url":"` + upstream.URL + `","api_key":"bad"}]`
	dir := t.TempDir()
	opts.config = filepath.Join(dir, "config.json")
	opts.credentials = filepath.Join(dir, "credentials.json")
	opts.freeModels = filepath.Join(dir, "free-models.json")
	if err := os.WriteFile(opts.freeModels, []byte(`{"schema_version":2,"generated_at":"test","providers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := discoverModelData(context.Background(), opts, "test", &output); err != nil {
		t.Fatal(err)
	}
	var result modelDiscoveryOutput
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Checked) != 0 || len(result.FetchFailures) != 1 {
		t.Fatalf("failed fetch must not be authoritative: %#v", result)
	}
	if result.Available["test"] {
		t.Fatalf("failed provider reported available: %#v", result.Available)
	}
}

func TestDiscoverModelDataReportsUnconfiguredOfficialCatalogFailure(t *testing.T) {
	for _, key := range provider.SupportedKeyEnvs() {
		t.Setenv(key, "")
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "missing token", http.StatusUnauthorized)
	}))
	defer upstream.Close()
	opts := defaultOptions()
	opts.providers = `[{"id":"unconfigured","base_url":"` + upstream.URL + `","api_key_env":"UNCONFIGURED_TEST_KEY","model_discovery":"api"}]`
	dir := t.TempDir()
	opts.config = filepath.Join(dir, "config.json")
	opts.credentials = filepath.Join(dir, "credentials.json")
	opts.freeModels = filepath.Join(dir, "free-models.json")
	if err := os.WriteFile(opts.freeModels, []byte(`{"schema_version":2,"generated_at":"test","providers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := discoverModelData(context.Background(), opts, "unconfigured", &output); err != nil {
		t.Fatal(err)
	}
	var result modelDiscoveryOutput
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Checked) != 0 || len(result.FetchFailures) != 1 {
		t.Fatalf("result = %#v", result)
	}
}
