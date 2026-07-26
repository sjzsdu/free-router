package provider

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

//go:embed free-models.json
var embeddedFreeModels []byte

var manifestPrice = regexp.MustCompile(`^(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$`)

type Spec struct {
	ID                  string            `json:"id"`
	BaseURL             string            `json:"base_url"`
	ModelsURL           string            `json:"models_url,omitempty"`
	ChatURL             string            `json:"chat_url,omitempty"`
	Endpoints           map[string]string `json:"endpoints,omitempty"`
	APIKey              string            `json:"api_key,omitempty"`
	APIKeyEnv           string            `json:"api_key_env,omitempty"`
	NoAuth              bool              `json:"no_auth,omitempty"`
	AuthHeader          string            `json:"auth_header,omitempty"`
	AuthPrefix          string            `json:"auth_prefix,omitempty"`
	Headers             map[string]string `json:"headers,omitempty"`
	Tier                string            `json:"tier,omitempty"`
	FreeKind            string            `json:"free_kind,omitempty"`
	BillingWarning      string            `json:"billing_warning,omitempty"`
	RegisterURL         string            `json:"register_url,omitempty"`
	RegisterLabel       string            `json:"register_label,omitempty"`
	OAuth               bool              `json:"oauth,omitempty"`
	UseNameAsID         bool              `json:"use_name_as_id,omitempty"`
	ModelDiscovery      string            `json:"model_discovery,omitempty"`
	FreeModelPolicy     string            `json:"free_model_policy,omitempty"`
	DiscoveredModels    []DiscoveredModel `json:"-"`
	FreeBasis           string            `json:"-"`
	SourceURLs          []string          `json:"-"`
	ManifestGeneratedAt string            `json:"-"`
	DiscoveryStatus     string            `json:"-"`
	DiscoveryMessage    string            `json:"-"`
	RequiredEnvs        []string          `json:"-"`
}

// DiscoveredModel is model metadata produced by the free-model discovery Formula.
// It deliberately contains no credentials or provider connection settings.
type DiscoveredModel struct {
	ID                  string            `json:"id"`
	Name                string            `json:"name,omitempty"`
	Description         string            `json:"description,omitempty"`
	OwnedBy             string            `json:"owned_by,omitempty"`
	Type                string            `json:"type,omitempty"`
	Functions           []string          `json:"functions,omitempty"`
	ContextLength       int               `json:"context_length,omitempty"`
	MaxOutputTokens     int               `json:"max_output_tokens,omitempty"`
	InputModalities     []string          `json:"input_modalities,omitempty"`
	OutputModalities    []string          `json:"output_modalities,omitempty"`
	SupportedParameters []string          `json:"supported_parameters,omitempty"`
	SupportedEndpoints  []string          `json:"supported_endpoints,omitempty"`
	FreeBasis           string            `json:"free_basis,omitempty"`
	SourceURLs          []string          `json:"source_urls,omitempty"`
	VerifiedAt          string            `json:"verified_at,omitempty"`
	Pricing             DiscoveredPricing `json:"pricing,omitempty"`
}

type DiscoveredPricing struct {
	Prompt     string `json:"prompt,omitempty"`
	Completion string `json:"completion,omitempty"`
}

type FreeModelManifest struct {
	SchemaVersion int                            `json:"schema_version"`
	GeneratedAt   string                         `json:"generated_at,omitempty"`
	Providers     map[string]FreeProviderCatalog `json:"providers"`
}

type FreeProviderCatalog struct {
	FreeBasis        string            `json:"free_basis,omitempty"`
	SourceURLs       []string          `json:"source_urls,omitempty"`
	Models           []DiscoveredModel `json:"models"`
	BillingWarning   string            `json:"billing_warning,omitempty"`
	DiscoveryStatus  string            `json:"discovery_status,omitempty"`
	DiscoveryMessage string            `json:"discovery_message,omitempty"`
}

func loadFreeModelManifest(path string) (FreeModelManifest, error) {
	content := embeddedFreeModels
	if strings.TrimSpace(path) != "" {
		var err error
		content, err = os.ReadFile(path)
		if err != nil {
			return FreeModelManifest{}, fmt.Errorf("read free model manifest: %w", err)
		}
	}
	var manifest FreeModelManifest
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return FreeModelManifest{}, fmt.Errorf("decode free model manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return FreeModelManifest{}, fmt.Errorf("decode free model manifest: %w", err)
	}
	if err := ValidateFreeModelManifest(manifest); err != nil {
		return FreeModelManifest{}, err
	}
	return manifest, nil
}

func LoadFreeModelManifest(path string) (FreeModelManifest, error) {
	return loadFreeModelManifest(path)
}

func ValidateFreeModelManifest(manifest FreeModelManifest) error {
	if manifest.SchemaVersion != 2 {
		return fmt.Errorf("unsupported free model manifest schema_version %d", manifest.SchemaVersion)
	}
	for providerID, entry := range manifest.Providers {
		if strings.TrimSpace(providerID) == "" {
			return errors.New("free model manifest contains an empty provider id")
		}
		if !validDiscoveryStatus(entry.DiscoveryStatus) {
			return fmt.Errorf("provider %s has unsupported discovery_status %q", providerID, entry.DiscoveryStatus)
		}
		seen := make(map[string]bool)
		for _, model := range entry.Models {
			if strings.TrimSpace(model.ID) == "" {
				return fmt.Errorf("provider %s contains a model with an empty id", providerID)
			}
			if seen[model.ID] {
				return fmt.Errorf("provider %s contains duplicate model %q", providerID, model.ID)
			}
			seen[model.ID] = true
			if len(model.Functions) == 0 {
				return fmt.Errorf("provider %s model %s has no functions", providerID, model.ID)
			}
			for _, function := range model.Functions {
				if !validModelFunction(function) {
					return fmt.Errorf("provider %s model %s has unsupported function %q", providerID, model.ID, function)
				}
			}
			for _, price := range []struct{ field, value string }{
				{field: "prompt", value: model.Pricing.Prompt},
				{field: "completion", value: model.Pricing.Completion},
			} {
				if err := validateManifestPrice(price.value); err != nil {
					return fmt.Errorf("provider %s model %s has invalid pricing.%s: %w", providerID, model.ID, price.field, err)
				}
			}
			if len(model.SourceURLs) == 0 && len(entry.SourceURLs) == 0 {
				return fmt.Errorf("provider %s model %s has no evidence source", providerID, model.ID)
			}
		}
	}
	return nil
}

func validDiscoveryStatus(status string) bool {
	switch status {
	case "", "ready", "confirmed-empty", "discovery-failed", "validation-failed", "verification-failed", "awaiting-approval":
		return true
	default:
		return false
	}
}

func validateManifestPrice(value string) error {
	if value == "" {
		return nil
	}
	if !manifestPrice.MatchString(value) {
		return fmt.Errorf("%q is not a non-negative numeric value", value)
	}
	if _, err := strconv.ParseFloat(value, 64); err != nil {
		return fmt.Errorf("%q is outside the supported numeric range", value)
	}
	return nil
}

func validModelFunction(function string) bool {
	switch function {
	case "chat", "chat-tools", "image-understanding", "image-generation", "video-understanding", "video-generation", "audio-understanding", "speech-to-text", "text-to-speech", "embedding", "rerank", "moderation":
		return true
	default:
		return false
	}
}

func applyFreeModelManifest(specs []Spec, manifest FreeModelManifest) []Spec {
	result := append([]Spec(nil), specs...)
	for index := range result {
		entry, ok := manifest.Providers[result[index].ID]
		if !ok {
			result[index].DiscoveredModels = nil
			continue
		}
		result[index].FreeBasis = entry.FreeBasis
		result[index].SourceURLs = append([]string(nil), entry.SourceURLs...)
		result[index].ManifestGeneratedAt = manifest.GeneratedAt
		result[index].DiscoveryStatus = entry.DiscoveryStatus
		result[index].DiscoveryMessage = entry.DiscoveryMessage
		if entry.BillingWarning != "" {
			result[index].BillingWarning = entry.BillingWarning
		}
		if discoveryCatalogEnabled(entry.DiscoveryStatus) {
			result[index].DiscoveredModels = append([]DiscoveredModel(nil), entry.Models...)
		} else {
			result[index].DiscoveredModels = nil
		}
	}
	return result
}

func discoveryCatalogEnabled(status string) bool {
	switch status {
	case "", "ready", "awaiting-approval":
		return true
	case "verification-failed":
		return false
	default:
		return true
	}
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
	catalog   map[string]Spec
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
	return newRegistry(customJSON, DefaultEnvMap(), resolvers...)
}

func NewRegistryWithEnv(customJSON string, envMap EnvMap, resolvers ...KeyResolver) (*Registry, error) {
	return newRegistry(customJSON, MergeEnvMap(envMap), resolvers...)
}

func NewRegistryWithManifest(customJSON string, envMap EnvMap, manifestPath string, resolvers ...KeyResolver) (*Registry, error) {
	manifest, err := loadFreeModelManifest(manifestPath)
	if err != nil {
		return nil, err
	}
	return newRegistryWithSpecs(customJSON, MergeEnvMap(envMap), builtins(), manifest, resolvers...)
}

func newRegistry(customJSON string, envMap EnvMap, resolvers ...KeyResolver) (*Registry, error) {
	manifest, err := loadFreeModelManifest("")
	if err != nil {
		return nil, err
	}
	return newRegistryWithSpecs(customJSON, envMap, builtins(), manifest, resolvers...)
}

func newRegistryWithSpecs(customJSON string, envMap EnvMap, specs []Spec, manifest FreeModelManifest, resolvers ...KeyResolver) (*Registry, error) {
	registry := &Registry{providers: make(map[string]Spec), catalog: make(map[string]Spec)}
	for _, spec := range applyFreeModelManifest(specs, manifest) {
		registry.catalog[spec.ID] = spec
		registry.addIfConfigured(spec, envMap, resolvers)
	}
	if strings.TrimSpace(customJSON) != "" {
		var custom []Spec
		if err := json.Unmarshal([]byte(customJSON), &custom); err != nil {
			return nil, fmt.Errorf("decode FREE_ROUTER_PROVIDERS: %w", err)
		}
		for _, spec := range applyFreeModelManifest(custom, manifest) {
			if spec.ID == "" || spec.BaseURL == "" {
				return nil, fmt.Errorf("custom provider requires id and base_url")
			}
			if spec.APIKey == "" {
				spec.APIKey, _, _ = resolveEnvironment(spec, envMap)
			}
			if spec.APIKey == "" {
				spec.APIKey, _ = resolveKey(spec.ID, resolvers)
			}
			registry.catalog[spec.ID] = spec
			registry.addIfConfigured(spec, envMap, resolvers)
		}
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

func (registry *Registry) CatalogGet(id string) (Spec, bool) {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	spec, ok := registry.catalog[id]
	return spec, ok
}

func (registry *Registry) CatalogAll() []Spec {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	result := make([]Spec, 0, len(registry.catalog))
	for _, spec := range registry.catalog {
		result = append(result, spec)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func (registry *Registry) Reload(customJSON string, resolvers ...KeyResolver) error {
	return registry.ReloadWithEnv(customJSON, DefaultEnvMap(), resolvers...)
}

func (registry *Registry) ReloadWithEnv(customJSON string, envMap EnvMap, resolvers ...KeyResolver) error {
	updated, err := newRegistry(customJSON, MergeEnvMap(envMap), resolvers...)
	if err != nil {
		return err
	}
	registry.mu.Lock()
	registry.providers = updated.providers
	registry.catalog = updated.catalog
	registry.mu.Unlock()
	return nil
}

func (registry *Registry) ReloadWithManifest(customJSON string, envMap EnvMap, manifestPath string, resolvers ...KeyResolver) error {
	updated, err := NewRegistryWithManifest(customJSON, envMap, manifestPath, resolvers...)
	if err != nil {
		return err
	}
	registry.mu.Lock()
	registry.providers = updated.providers
	registry.catalog = updated.catalog
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
	return BuiltinStatusWithManifest(envMap, "", resolvers...)
}

func BuiltinStatusWithManifest(envMap EnvMap, manifestPath string, resolvers ...KeyResolver) []map[string]any {
	envMap = MergeEnvMap(envMap)
	specs := builtins()
	manifest, manifestErr := loadFreeModelManifest(manifestPath)
	if manifestErr == nil {
		specs = applyFreeModelManifest(specs, manifest)
	}
	result := make([]map[string]any, 0, len(specs))
	for _, spec := range specs {
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
		catalogStatus := "empty"
		if len(spec.DiscoveredModels) > 0 {
			catalogStatus = "ready"
		}
		discoveryStatus := spec.DiscoveryStatus
		discoveryMessage := spec.DiscoveryMessage
		if manifestErr != nil {
			discoveryStatus = "manifest-error"
			discoveryMessage = manifestErr.Error()
		} else if discoveryStatus == "" {
			if len(spec.DiscoveredModels) > 0 {
				discoveryStatus = "ready"
				discoveryMessage = "Formula 清单已收录可路由模型"
			} else {
				discoveryStatus = "awaiting-discovery"
				discoveryMessage = "当前清单尚未记录最近一次 Formula 发现结论"
			}
		}
		result = append(result, map[string]any{
			"id": spec.ID, "envs": effectiveEnvNames(spec, envMap), "matched_env": matchedEnv,
			"requires": spec.RequiredEnvs, "missing_required": missingRequired,
			"configured": configured, "source": source, "tier": spec.Tier, "free_kind": spec.FreeKind,
			"billing_warning": spec.BillingWarning, "register_url": spec.RegisterURL, "register_label": spec.RegisterLabel, "oauth": spec.OAuth,
			"catalog_status": catalogStatus, "formula_model_count": len(spec.DiscoveredModels), "free_basis": spec.FreeBasis, "source_urls": spec.SourceURLs,
			"discovery_status": discoveryStatus, "discovery_message": discoveryMessage,
			"model_discovery": spec.ModelDiscovery, "free_model_policy": spec.FreeModelPolicy,
			"manifest_generated_at": spec.ManifestGeneratedAt, "manifest_error": errorString(manifestErr),
		})
	}
	return result
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
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
		{ID: "openrouter", BaseURL: "https://openrouter.ai/api/v1", APIKeyEnv: "OPENROUTER_API_KEY", ModelDiscovery: "api", FreeModelPolicy: "zero-price", Tier: "zero-price-models", RegisterURL: "https://openrouter.ai/keys", OAuth: true},
		{ID: "groq", BaseURL: "https://api.groq.com/openai/v1", APIKeyEnv: "GROQ_API_KEY", ModelDiscovery: "api", FreeModelPolicy: "all", Tier: "free-tier", RegisterURL: "https://console.groq.com/keys"},
		{ID: "cerebras", BaseURL: "https://api.cerebras.ai/v1", APIKeyEnv: "CEREBRAS_API_KEY", ModelDiscovery: "api", FreeModelPolicy: "all", Tier: "free-tier", RegisterURL: "https://cloud.cerebras.ai/"},
		{ID: "gemini", BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai", APIKeyEnv: "GEMINI_API_KEY", ModelDiscovery: "api-agent-filter", FreeModelPolicy: "all", Tier: "free-tier", RegisterURL: "https://aistudio.google.com/apikey"},
		{ID: "github-models", BaseURL: "https://models.github.ai/inference", APIKeyEnv: "GITHUB_TOKEN", Headers: map[string]string{"Accept": "application/vnd.github+json", "X-GitHub-Api-Version": "2022-11-28"}, ModelDiscovery: "api", FreeModelPolicy: "all", Tier: "free-tier", RegisterURL: "https://github.com/settings/personal-access-tokens/new"},
		{ID: "pollinations", BaseURL: "https://gen.pollinations.ai/v1", APIKeyEnv: "POLLINATIONS_API_KEY", ModelDiscovery: "api-agent-filter", FreeModelPolicy: "all", Tier: "free-credits", RegisterURL: "https://enter.pollinations.ai/"},
		{ID: "huggingface", BaseURL: "https://router.huggingface.co/v1", APIKeyEnv: "HF_TOKEN", ModelDiscovery: "api", FreeModelPolicy: "all", Tier: "free-credits", RegisterURL: "https://huggingface.co/settings/tokens"},
		{ID: "nvidia", BaseURL: "https://integrate.api.nvidia.com/v1", APIKeyEnv: "NVIDIA_API_KEY", ModelDiscovery: "api", FreeModelPolicy: "all", Tier: "free-credits", RegisterURL: "https://build.nvidia.com/settings/api-keys"},
		{ID: "mistral", BaseURL: "https://api.mistral.ai/v1", APIKeyEnv: "MISTRAL_API_KEY", ModelDiscovery: "api", FreeModelPolicy: "all", Tier: "free-experiment-plan", RegisterURL: "https://console.mistral.ai/home?profile_dialog=api-keys"},
		{ID: "sambanova", BaseURL: "https://api.sambanova.ai/v1", APIKeyEnv: "SAMBANOVA_API_KEY", ModelDiscovery: "api", FreeModelPolicy: "all", Tier: "free-tier", RegisterURL: "https://cloud.sambanova.ai/apis"},
		{ID: "ollama-cloud", BaseURL: "https://api.ollama.com/v1", APIKeyEnv: "OLLAMA_API_KEY", ModelDiscovery: "api", FreeModelPolicy: "all", Tier: "free-tier", RegisterURL: "https://ollama.com/settings/keys"},
		{ID: "modelscope", BaseURL: "https://api-inference.modelscope.cn/v1", APIKeyEnv: "MODELSCOPE_API_KEY", ModelDiscovery: "api", FreeModelPolicy: "all", Tier: "free-tier", RegisterURL: "https://modelscope.cn/my/myaccesstoken"},
		{
			ID: "xiaomi-mimo", BaseURL: "https://api.xiaomimimo.com/v1", APIKeyEnv: "MIMO_API_KEY", ModelDiscovery: "api", FreeModelPolicy: "all",
			Tier: "gift-credits", FreeKind: "credit", BillingWarning: "赠送额度用完后停止使用或按账户计费设置执行，请先确认余额。",
			RegisterURL: "https://platform.xiaomimimo.com/#/console/api-keys",
		},
		{
			ID: "dashscope", BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1", APIKeyEnv: "DASHSCOPE_API_KEY", ModelDiscovery: "api-agent-filter", FreeModelPolicy: "all",
			Tier: "new-user-free-quota", FreeKind: "trial", BillingWarning: "新人额度通常仅 30～90 天；请在百炼控制台开启免费额度用完即停。",
			RegisterURL: "https://bailian.console.aliyun.com/cn-beijing/?tab=app#/api-key",
		},
		{
			ID: "volcengine-ark", BaseURL: "https://ark.cn-beijing.volces.com/api/v3", APIKeyEnv: "ARK_API_KEY", ModelDiscovery: "agent",
			Tier: "free-trial-quota", FreeKind: "trial", BillingWarning: "免费额度按模型限量发放，耗尽后可能转为计费。",
			RegisterURL: "https://console.volcengine.com/ark/region:ark+cn-beijing/apikey",
		},
		{
			ID: "baichuan", BaseURL: "https://api.baichuan-ai.com/v1", APIKeyEnv: "BAICHUAN_API_KEY", ModelDiscovery: "agent",
			Tier: "new-user-gift-credit", FreeKind: "credit", BillingWarning: "新用户赠送金有效期有限，余额耗尽后接口按平台价格计费。",
			RegisterURL: "https://platform.baichuan-ai.com/homePage", RegisterLabel: "申请接入",
		},
		{
			ID: "bigmodel", BaseURL: "https://open.bigmodel.cn/api/paas/v4", ModelDiscovery: "agent",
			APIKeyEnv: "BIGMODEL_API_KEY", Tier: "free-flash-models", RegisterURL: "https://bigmodel.cn/usercenter/proj-mgmt/apikeys",
		},
		{
			ID: "qianfan", BaseURL: "https://qianfan.baidubce.com/v2", ModelDiscovery: "api", FreeModelPolicy: "zero-price",
			APIKeyEnv: "QIANFAN_API_KEY", Tier: "long-term-free-models", RegisterURL: "https://console.bce.baidu.com/qianfan/ais/console/apiKey",
		},
		{
			ID: "siliconflow", BaseURL: "https://api.siliconflow.cn/v1",
			ModelsURL: "https://api.siliconflow.cn/v1/models?type=text&sub_type=chat", ModelDiscovery: "api-agent-filter", FreeModelPolicy: "all",
			APIKeyEnv: "SILICONFLOW_API_KEY", Tier: "free-models", RegisterURL: "https://cloud.siliconflow.cn/account/ak",
		},
		{ID: "zai", BaseURL: "https://api.z.ai/api/paas/v4", APIKeyEnv: "ZAI_API_KEY", ModelDiscovery: "api-agent-filter", FreeModelPolicy: "all", Tier: "free-models", RegisterURL: "https://z.ai/manage-apikey/apikey-list"},
		{ID: "cloudflare", BaseURL: "https://api.cloudflare.com/client/v4/accounts/" + cloudflareAccount + "/ai/v1", ModelsURL: "https://api.cloudflare.com/client/v4/accounts/" + cloudflareAccount + "/ai/models/search", APIKeyEnv: "CLOUDFLARE_API_TOKEN", RequiredEnvs: []string{"CLOUDFLARE_ACCOUNT_ID"}, UseNameAsID: true, ModelDiscovery: "api", FreeModelPolicy: "all", Tier: "10000-neurons-per-day", RegisterURL: "https://dash.cloudflare.com/?to=%2F%3Aaccount%2Fai%2Fworkers-ai"},
	}
}
