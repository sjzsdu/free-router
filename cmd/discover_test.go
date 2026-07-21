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

func TestDiscoverModelDataKeepsOnlySuccessfulCapabilities(t *testing.T) {
	for _, key := range provider.SupportedKeyEnvs() {
		t.Setenv(key, "")
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"working","supported_parameters":["tools"]},{"id":"broken"}]}`))
		case "/chat/completions":
			var input struct {
				Model string `json:"model"`
				Tools []any  `json:"tools"`
			}
			_ = json.NewDecoder(r.Body).Decode(&input)
			if input.Model == "broken" || len(input.Tools) > 0 {
				http.Error(w, "unsupported", http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	opts := defaultOptions()
	opts.providers = `[{"id":"test","base_url":"` + upstream.URL + `","no_auth":true}]`
	dir := t.TempDir()
	opts.config = filepath.Join(dir, "config.json")
	opts.credentials = filepath.Join(dir, "credentials.json")
	opts.freeModels = filepath.Join(dir, "free-models.json")
	if err := os.WriteFile(opts.freeModels, []byte(`{"schema_version":1,"generated_at":"test","providers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := discoverModelData(context.Background(), opts, &output); err != nil {
		t.Fatal(err)
	}
	var result modelDiscoveryOutput
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Providers) != 1 || len(result.Providers[0].Models) != 1 {
		t.Fatalf("result=%#v", result)
	}
	model := result.Providers[0].Models[0]
	if model.ID != "working" || len(model.Functions) != 1 || model.Functions[0] != "chat" {
		t.Fatalf("model=%#v", model)
	}
	if len(result.ProbeFailures) != 2 {
		t.Fatalf("probe failures=%#v", result.ProbeFailures)
	}
}
