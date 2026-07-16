package provider

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

type Filter string

const (
	FilterAll       Filter = "all"
	FilterZeroPrice Filter = "zero-price"
)

type Spec struct {
	ID            string            `json:"id"`
	BaseURL       string            `json:"base_url"`
	ModelsURL     string            `json:"models_url,omitempty"`
	ChatURL       string            `json:"chat_url,omitempty"`
	APIKey        string            `json:"api_key,omitempty"`
	APIKeyEnv     string            `json:"api_key_env,omitempty"`
	NoAuth        bool              `json:"no_auth,omitempty"`
	AuthHeader    string            `json:"auth_header,omitempty"`
	AuthPrefix    string            `json:"auth_prefix,omitempty"`
	Headers       map[string]string `json:"headers,omitempty"`
	Filter        Filter            `json:"filter,omitempty"`
	Tier          string            `json:"tier,omitempty"`
	UseNameAsID   bool              `json:"use_name_as_id,omitempty"`
	AllowedModels []string          `json:"allowed_models,omitempty"`
	RequiredEnvs  []string          `json:"-"`
}

func (spec Spec) ModelsEndpoint() string {
	if spec.ModelsURL != "" {
		return spec.ModelsURL
	}
	return strings.TrimRight(spec.BaseURL, "/") + "/models"
}

func (spec Spec) ChatEndpoint() string {
	if spec.ChatURL != "" {
		return spec.ChatURL
	}
	return strings.TrimRight(spec.BaseURL, "/") + "/chat/completions"
}

func (spec Spec) ApplyAuth(headers map[string]string) {
	if spec.APIKey == "" || spec.NoAuth {
		return
	}
	header := spec.AuthHeader
	if header == "" {
		header = "Authorization"
	}
	prefix := spec.AuthPrefix
	if prefix == "" {
		prefix = "Bearer "
	}
	headers[header] = prefix + spec.APIKey
}

type Registry struct {
	providers map[string]Spec
}

func NewRegistry(customJSON string) (*Registry, error) {
	registry := &Registry{providers: make(map[string]Spec)}
	for _, spec := range builtins() {
		registry.addIfConfigured(spec)
	}
	if strings.TrimSpace(customJSON) != "" {
		var custom []Spec
		if err := json.Unmarshal([]byte(customJSON), &custom); err != nil {
			return nil, fmt.Errorf("decode FREE_ROUTER_PROVIDERS: %w", err)
		}
		for _, spec := range custom {
			if spec.ID == "" || spec.BaseURL == "" {
				return nil, fmt.Errorf("custom provider requires id and base_url")
			}
			if spec.APIKey == "" && spec.APIKeyEnv != "" {
				spec.APIKey = os.Getenv(spec.APIKeyEnv)
			}
			if spec.APIKey == "" && !spec.NoAuth {
				return nil, fmt.Errorf("custom provider %q has no API key; set api_key_env or no_auth", spec.ID)
			}
			registry.addIfConfigured(spec)
		}
	}
	if len(registry.providers) == 0 {
		return nil, fmt.Errorf("no free provider configured; set one of %s", strings.Join(SupportedKeyEnvs(), ", "))
	}
	return registry, nil
}

func (registry *Registry) Get(id string) (Spec, bool) {
	spec, ok := registry.providers[id]
	return spec, ok
}

func (registry *Registry) All() []Spec {
	result := make([]Spec, 0, len(registry.providers))
	for _, spec := range registry.providers {
		result = append(result, spec)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func (registry *Registry) addIfConfigured(spec Spec) {
	if spec.APIKey == "" && spec.APIKeyEnv != "" {
		spec.APIKey = os.Getenv(spec.APIKeyEnv)
	}
	if spec.APIKey == "" && !spec.NoAuth {
		return
	}
	for _, key := range spec.RequiredEnvs {
		if os.Getenv(key) == "" {
			return
		}
	}
	if spec.Filter == "" {
		spec.Filter = FilterAll
	}
	if spec.Headers == nil {
		spec.Headers = map[string]string{}
	}
	registry.providers[spec.ID] = spec
}

func SupportedKeyEnvs() []string {
	result := make([]string, 0, len(builtins()))
	for _, spec := range builtins() {
		result = append(result, spec.APIKeyEnv)
	}
	sort.Strings(result)
	return result
}

func BuiltinStatus() []map[string]any {
	result := make([]map[string]any, 0, len(builtins()))
	for _, spec := range builtins() {
		configured := os.Getenv(spec.APIKeyEnv) != ""
		for _, key := range spec.RequiredEnvs {
			configured = configured && os.Getenv(key) != ""
		}
		result = append(result, map[string]any{
			"id": spec.ID, "env": spec.APIKeyEnv, "requires": spec.RequiredEnvs, "configured": configured, "tier": spec.Tier,
		})
	}
	return result
}

func builtins() []Spec {
	cloudflareAccount := os.Getenv("CLOUDFLARE_ACCOUNT_ID")
	return []Spec{
		{ID: "openrouter", BaseURL: "https://openrouter.ai/api/v1", APIKeyEnv: "OPENROUTER_API_KEY", Filter: FilterZeroPrice, Tier: "zero-price-models"},
		{ID: "groq", BaseURL: "https://api.groq.com/openai/v1", APIKeyEnv: "GROQ_API_KEY", Tier: "free-tier"},
		{ID: "cerebras", BaseURL: "https://api.cerebras.ai/v1", APIKeyEnv: "CEREBRAS_API_KEY", Tier: "free-tier"},
		{ID: "gemini", BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai", APIKeyEnv: "GEMINI_API_KEY", Tier: "free-tier"},
		{ID: "github-models", BaseURL: "https://models.github.ai/inference", APIKeyEnv: "GITHUB_TOKEN", Headers: map[string]string{"Accept": "application/vnd.github+json", "X-GitHub-Api-Version": "2022-11-28"}, Tier: "free-tier"},
		{ID: "pollinations", BaseURL: "https://gen.pollinations.ai/v1", APIKeyEnv: "POLLINATIONS_API_KEY", Tier: "free-credits"},
		{ID: "huggingface", BaseURL: "https://router.huggingface.co/v1", APIKeyEnv: "HF_TOKEN", Tier: "free-credits"},
		{ID: "nvidia", BaseURL: "https://integrate.api.nvidia.com/v1", APIKeyEnv: "NVIDIA_API_KEY", Tier: "free-credits"},
		{ID: "mistral", BaseURL: "https://api.mistral.ai/v1", APIKeyEnv: "MISTRAL_API_KEY", Tier: "free-experiment-plan"},
		{ID: "sambanova", BaseURL: "https://api.sambanova.ai/v1", APIKeyEnv: "SAMBANOVA_API_KEY", Tier: "free-tier"},
		{ID: "ollama-cloud", BaseURL: "https://api.ollama.com/v1", APIKeyEnv: "OLLAMA_API_KEY", Tier: "free-tier"},
		{ID: "modelscope", BaseURL: "https://api-inference.modelscope.cn/v1", APIKeyEnv: "MODELSCOPE_API_KEY", Tier: "free-tier"},
		{
			ID: "siliconflow", BaseURL: "https://api.siliconflow.cn/v1",
			ModelsURL: "https://api.siliconflow.cn/v1/models?type=text&sub_type=chat",
			APIKeyEnv: "SILICONFLOW_API_KEY", Tier: "free-models",
			AllowedModels: []string{
				"Qwen/Qwen3.5-4B",
				"PaddlePaddle/PaddleOCR-VL-1.5",
				"Qwen/Qwen3-8B",
				"Qwen/Qwen2.5-7B-Instruct",
				"THUDM/GLM-4-9B-0414",
				"THUDM/GLM-Z1-9B-0414",
				"deepseek-ai/DeepSeek-OCR",
				"deepseek-ai/DeepSeek-R1-0528-Qwen3-8B",
				"tencent/Hunyuan-MT-7B",
			},
		},
		{ID: "zai", BaseURL: "https://api.z.ai/api/paas/v4", APIKeyEnv: "ZAI_API_KEY", Tier: "free-models"},
		{ID: "cloudflare", BaseURL: "https://api.cloudflare.com/client/v4/accounts/" + cloudflareAccount + "/ai/v1", ModelsURL: "https://api.cloudflare.com/client/v4/accounts/" + cloudflareAccount + "/ai/models/search", APIKeyEnv: "CLOUDFLARE_API_TOKEN", RequiredEnvs: []string{"CLOUDFLARE_ACCOUNT_ID"}, UseNameAsID: true, Tier: "10000-neurons-per-day"},
	}
}
