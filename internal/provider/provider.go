package provider

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
)

type Filter string

const (
	FilterAll       Filter = "all"
	FilterZeroPrice Filter = "zero-price"
)

type Spec struct {
	ID                   string            `json:"id"`
	BaseURL              string            `json:"base_url"`
	ModelsURL            string            `json:"models_url,omitempty"`
	ChatURL              string            `json:"chat_url,omitempty"`
	Endpoints            map[string]string `json:"endpoints,omitempty"`
	APIKey               string            `json:"api_key,omitempty"`
	APIKeyEnv            string            `json:"api_key_env,omitempty"`
	NoAuth               bool              `json:"no_auth,omitempty"`
	AuthHeader           string            `json:"auth_header,omitempty"`
	AuthPrefix           string            `json:"auth_prefix,omitempty"`
	Headers              map[string]string `json:"headers,omitempty"`
	Filter               Filter            `json:"filter,omitempty"`
	Tier                 string            `json:"tier,omitempty"`
	FreeKind             string            `json:"free_kind,omitempty"`
	BillingWarning       string            `json:"billing_warning,omitempty"`
	RegisterURL          string            `json:"register_url,omitempty"`
	OAuth                bool              `json:"oauth,omitempty"`
	UseNameAsID          bool              `json:"use_name_as_id,omitempty"`
	AllowedModels        []string          `json:"allowed_models,omitempty"`
	AllowedModelPatterns []string          `json:"allowed_model_patterns,omitempty"`
	RequiredEnvs         []string          `json:"-"`
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

func (spec Spec) APIEndpoint(path string) string {
	if endpoint := spec.Endpoints[path]; endpoint != "" {
		return endpoint
	}
	if path == "/chat/completions" {
		return spec.ChatEndpoint()
	}
	return strings.TrimRight(spec.BaseURL, "/") + "/" + strings.TrimLeft(path, "/")
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
	mu        sync.RWMutex
	providers map[string]Spec
}

type KeyResolver func(providerID string) (string, bool)

type EnvMap map[string][]string

func DefaultEnvMap() EnvMap {
	result := make(EnvMap)
	for _, spec := range builtins() {
		if spec.APIKeyEnv != "" {
			result[spec.ID] = []string{spec.APIKeyEnv}
		}
	}
	return result
}

func MergeEnvMap(custom EnvMap) EnvMap {
	result := make(EnvMap)
	defaults := DefaultEnvMap()
	for providerID, names := range custom {
		result[providerID] = mergeEnvNames(names, defaults[providerID])
	}
	for providerID, names := range defaults {
		if _, ok := result[providerID]; !ok {
			result[providerID] = append([]string{}, names...)
		}
	}
	return result
}

func NewRegistry(customJSON string, resolvers ...KeyResolver) (*Registry, error) {
	return newRegistry(customJSON, false, DefaultEnvMap(), resolvers...)
}

func NewRegistryAllowEmpty(customJSON string, resolvers ...KeyResolver) (*Registry, error) {
	return newRegistry(customJSON, true, DefaultEnvMap(), resolvers...)
}

func NewRegistryWithEnv(customJSON string, envMap EnvMap, resolvers ...KeyResolver) (*Registry, error) {
	return newRegistry(customJSON, false, MergeEnvMap(envMap), resolvers...)
}

func NewRegistryAllowEmptyWithEnv(customJSON string, envMap EnvMap, resolvers ...KeyResolver) (*Registry, error) {
	return newRegistry(customJSON, true, MergeEnvMap(envMap), resolvers...)
}

func newRegistry(customJSON string, allowEmpty bool, envMap EnvMap, resolvers ...KeyResolver) (*Registry, error) {
	registry := &Registry{providers: make(map[string]Spec)}
	for _, spec := range builtins() {
		registry.addIfConfigured(spec, envMap, resolvers)
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
			if spec.APIKey == "" {
				spec.APIKey, _, _ = resolveEnvironment(spec, envMap)
			}
			if spec.APIKey == "" {
				spec.APIKey, _ = resolveKey(spec.ID, resolvers)
			}
			if spec.APIKey == "" && !spec.NoAuth {
				if allowEmpty {
					continue
				}
				return nil, fmt.Errorf("custom provider %q has no API key; set api_key_env or no_auth", spec.ID)
			}
			registry.addIfConfigured(spec, envMap, resolvers)
		}
	}
	if len(registry.providers) == 0 && !allowEmpty {
		return nil, fmt.Errorf("no free provider configured; run free-router setup or set one of %s", strings.Join(SupportedKeyEnvs(), ", "))
	}
	return registry, nil
}

func (registry *Registry) Get(id string) (Spec, bool) {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	spec, ok := registry.providers[id]
	return spec, ok
}

func (registry *Registry) All() []Spec {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	result := make([]Spec, 0, len(registry.providers))
	for _, spec := range registry.providers {
		result = append(result, spec)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func (registry *Registry) Reload(customJSON string, resolvers ...KeyResolver) error {
	return registry.ReloadWithEnv(customJSON, DefaultEnvMap(), resolvers...)
}

func (registry *Registry) ReloadWithEnv(customJSON string, envMap EnvMap, resolvers ...KeyResolver) error {
	updated, err := newRegistry(customJSON, true, MergeEnvMap(envMap), resolvers...)
	if err != nil {
		return err
	}
	registry.mu.Lock()
	registry.providers = updated.providers
	registry.mu.Unlock()
	return nil
}

func (registry *Registry) addIfConfigured(spec Spec, envMap EnvMap, resolvers []KeyResolver) {
	if spec.APIKey == "" {
		spec.APIKey, _, _ = resolveEnvironment(spec, envMap)
	}
	if spec.APIKey == "" {
		spec.APIKey, _ = resolveKey(spec.ID, resolvers)
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

func EnvironmentNames(custom EnvMap) []string {
	merged := MergeEnvMap(custom)
	seen := make(map[string]bool)
	result := make([]string, 0)
	for _, names := range merged {
		for _, name := range names {
			if !seen[name] {
				seen[name] = true
				result = append(result, name)
			}
		}
	}
	for _, spec := range builtins() {
		for _, name := range spec.RequiredEnvs {
			if !seen[name] {
				seen[name] = true
				result = append(result, name)
			}
		}
	}
	sort.Strings(result)
	return result
}

func BuiltinStatus(resolvers ...KeyResolver) []map[string]any {
	return BuiltinStatusWithEnv(DefaultEnvMap(), resolvers...)
}

func BuiltinStatusWithEnv(envMap EnvMap, resolvers ...KeyResolver) []map[string]any {
	envMap = MergeEnvMap(envMap)
	result := make([]map[string]any, 0, len(builtins()))
	for _, spec := range builtins() {
		_, matchedEnv, configured := resolveEnvironment(spec, envMap)
		source := "environment"
		if !configured {
			_, configured = resolveKey(spec.ID, resolvers)
			source = "saved"
		}
		if !configured {
			source = "missing"
		}
		missingRequired := make([]string, 0)
		for _, key := range spec.RequiredEnvs {
			if os.Getenv(key) == "" {
				missingRequired = append(missingRequired, key)
				configured = false
			}
		}
		result = append(result, map[string]any{
			"id": spec.ID, "envs": effectiveEnvNames(spec, envMap), "matched_env": matchedEnv,
			"requires": spec.RequiredEnvs, "missing_required": missingRequired,
			"configured": configured, "source": source, "tier": spec.Tier, "free_kind": spec.FreeKind,
			"billing_warning": spec.BillingWarning, "register_url": spec.RegisterURL, "oauth": spec.OAuth,
		})
	}
	return result
}

func resolveEnvironment(spec Spec, envMap EnvMap) (string, string, bool) {
	for _, name := range effectiveEnvNames(spec, envMap) {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value, name, true
		}
	}
	return "", "", false
}

func effectiveEnvNames(spec Spec, envMap EnvMap) []string {
	if names := envMap[spec.ID]; len(names) > 0 {
		return names
	}
	if spec.APIKeyEnv != "" {
		return []string{spec.APIKeyEnv}
	}
	return nil
}

func mergeEnvNames(groups ...[]string) []string {
	seen := make(map[string]bool)
	result := make([]string, 0)
	for _, group := range groups {
		for _, name := range group {
			name = strings.TrimSpace(name)
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			result = append(result, name)
		}
	}
	return result
}

func resolveKey(providerID string, resolvers []KeyResolver) (string, bool) {
	for _, resolver := range resolvers {
		if resolver == nil {
			continue
		}
		if key, ok := resolver(providerID); ok && key != "" {
			return key, true
		}
	}
	return "", false
}

func builtins() []Spec {
	cloudflareAccount := os.Getenv("CLOUDFLARE_ACCOUNT_ID")
	return []Spec{
		{ID: "openrouter", BaseURL: "https://openrouter.ai/api/v1", APIKeyEnv: "OPENROUTER_API_KEY", Filter: FilterZeroPrice, Tier: "zero-price-models", RegisterURL: "https://openrouter.ai/keys", OAuth: true},
		{ID: "groq", BaseURL: "https://api.groq.com/openai/v1", APIKeyEnv: "GROQ_API_KEY", Tier: "free-tier", RegisterURL: "https://console.groq.com/keys"},
		{ID: "cerebras", BaseURL: "https://api.cerebras.ai/v1", APIKeyEnv: "CEREBRAS_API_KEY", Tier: "free-tier", RegisterURL: "https://cloud.cerebras.ai/"},
		{ID: "gemini", BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai", APIKeyEnv: "GEMINI_API_KEY", Tier: "free-tier", RegisterURL: "https://aistudio.google.com/apikey"},
		{ID: "github-models", BaseURL: "https://models.github.ai/inference", APIKeyEnv: "GITHUB_TOKEN", Headers: map[string]string{"Accept": "application/vnd.github+json", "X-GitHub-Api-Version": "2022-11-28"}, Tier: "free-tier", RegisterURL: "https://github.com/settings/personal-access-tokens/new"},
		{ID: "pollinations", BaseURL: "https://gen.pollinations.ai/v1", APIKeyEnv: "POLLINATIONS_API_KEY", Tier: "free-credits", RegisterURL: "https://enter.pollinations.ai/"},
		{ID: "huggingface", BaseURL: "https://router.huggingface.co/v1", APIKeyEnv: "HF_TOKEN", Tier: "free-credits", RegisterURL: "https://huggingface.co/settings/tokens"},
		{ID: "nvidia", BaseURL: "https://integrate.api.nvidia.com/v1", APIKeyEnv: "NVIDIA_API_KEY", Tier: "free-credits", RegisterURL: "https://build.nvidia.com/settings/api-keys"},
		{ID: "mistral", BaseURL: "https://api.mistral.ai/v1", APIKeyEnv: "MISTRAL_API_KEY", Tier: "free-experiment-plan", RegisterURL: "https://console.mistral.ai/api-keys"},
		{ID: "sambanova", BaseURL: "https://api.sambanova.ai/v1", APIKeyEnv: "SAMBANOVA_API_KEY", Tier: "free-tier", RegisterURL: "https://cloud.sambanova.ai/apis"},
		{ID: "ollama-cloud", BaseURL: "https://api.ollama.com/v1", APIKeyEnv: "OLLAMA_API_KEY", Tier: "free-tier", RegisterURL: "https://ollama.com/settings/keys"},
		{ID: "modelscope", BaseURL: "https://api-inference.modelscope.cn/v1", APIKeyEnv: "MODELSCOPE_API_KEY", Tier: "free-tier", RegisterURL: "https://modelscope.cn/my/myaccesstoken"},
		{
			ID: "xiaomi-mimo", BaseURL: "https://api.xiaomimimo.com/v1", APIKeyEnv: "MIMO_API_KEY",
			Tier: "gift-credits", FreeKind: "credit", BillingWarning: "赠送额度用完后停止使用或按账户计费设置执行，请先确认余额。",
			RegisterURL: "https://platform.xiaomimimo.com/#/console/api-keys",
		},
		{
			ID: "dashscope", BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1", APIKeyEnv: "DASHSCOPE_API_KEY",
			Tier: "new-user-free-quota", FreeKind: "trial", BillingWarning: "新人额度通常仅 30～90 天；请在百炼控制台开启免费额度用完即停。",
			RegisterURL: "https://bailian.console.aliyun.com/?apiKey=1#/api-key",
		},
		{
			ID: "volcengine-ark", BaseURL: "https://ark.cn-beijing.volces.com/api/v3", APIKeyEnv: "ARK_API_KEY",
			Tier: "free-trial-quota", FreeKind: "trial", BillingWarning: "免费额度按模型限量发放，耗尽后可能转为计费。",
			RegisterURL: "https://console.volcengine.com/ark/region:ark+cn-beijing/apiKey",
		},
		{
			ID: "baichuan", BaseURL: "https://api.baichuan-ai.com/v1", APIKeyEnv: "BAICHUAN_API_KEY",
			Tier: "new-user-gift-credit", FreeKind: "credit", BillingWarning: "新用户赠送金有效期有限，余额耗尽后接口按平台价格计费。",
			RegisterURL: "https://platform.baichuan-ai.com/console/apikey",
		},
		{
			ID: "bigmodel", BaseURL: "https://open.bigmodel.cn/api/paas/v4",
			APIKeyEnv: "BIGMODEL_API_KEY", Tier: "free-flash-models", RegisterURL: "https://bigmodel.cn/usercenter/proj-mgmt/apikeys",
			AllowedModelPatterns: []string{"*flash*"},
		},
		{
			ID: "qianfan", BaseURL: "https://qianfan.baidubce.com/v2",
			APIKeyEnv: "QIANFAN_API_KEY", Tier: "long-term-free-models", RegisterURL: "https://console.bce.baidu.com/qianfan/ais/console/apiKey",
			AllowedModels: []string{"ernie-speed-8k", "ernie-speed-128k", "ernie-lite-8k", "ernie-tiny-8k"},
		},
		{
			ID: "siliconflow", BaseURL: "https://api.siliconflow.cn/v1",
			ModelsURL: "https://api.siliconflow.cn/v1/models?type=text&sub_type=chat",
			APIKeyEnv: "SILICONFLOW_API_KEY", Tier: "free-models", RegisterURL: "https://cloud.siliconflow.cn/account/ak",
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
		{ID: "zai", BaseURL: "https://api.z.ai/api/paas/v4", APIKeyEnv: "ZAI_API_KEY", Tier: "free-models", RegisterURL: "https://z.ai/manage-apikey/apikey-list"},
		{ID: "cloudflare", BaseURL: "https://api.cloudflare.com/client/v4/accounts/" + cloudflareAccount + "/ai/v1", ModelsURL: "https://api.cloudflare.com/client/v4/accounts/" + cloudflareAccount + "/ai/models/search", APIKeyEnv: "CLOUDFLARE_API_TOKEN", RequiredEnvs: []string{"CLOUDFLARE_ACCOUNT_ID"}, UseNameAsID: true, Tier: "10000-neurons-per-day", RegisterURL: "https://dash.cloudflare.com/profile/api-tokens"},
	}
}
