# free-router

一个极简、OpenAI 兼容的多免费源模型路由器：自动发现模型，跨 provider 故障切换，不维护静态模型名。

## 支持的免费源

程序会根据已保存凭据或环境变量自动启用 provider，没有配置密钥的源不会访问。

| Provider | 环境变量 | 免费形式 |
| --- | --- | --- |
| OpenRouter | `OPENROUTER_API_KEY` | 严格选择输入、输出价格均为 0 的模型 |
| Groq | `GROQ_API_KEY` | Free Plan |
| Cerebras | `CEREBRAS_API_KEY` | Free Tier |
| Google Gemini | `GEMINI_API_KEY` | Free Tier |
| GitHub Models | `GITHUB_TOKEN` | 免费原型额度 |
| Pollinations | `POLLINATIONS_API_KEY` | 免费 credits / Pollen |
| Hugging Face | `HF_TOKEN` | 每月免费 credits |
| NVIDIA NIM | `NVIDIA_API_KEY` | 免费 credits |
| Mistral | `MISTRAL_API_KEY` | Experiment Plan |
| SambaNova | `SAMBANOVA_API_KEY` | Free Tier |
| Ollama Cloud | `OLLAMA_API_KEY` | Free Tier |
| ModelScope | `MODELSCOPE_API_KEY` | 免费推理额度 |
| SiliconFlow | `SILICONFLOW_API_KEY` | 官方标记免费的聊天模型 |
| Z.AI | `ZAI_API_KEY` | GLM Flash 免费模型 |
| Cloudflare Workers AI | `CLOUDFLARE_API_TOKEN` + `CLOUDFLARE_ACCOUNT_ID` | 每日 10,000 Neurons |

这些服务的条款、区域和额度会变化。除 OpenRouter 的零价模型筛选外，provider 通常不会通过 `/models` 告诉路由器账户是否启用了付费；要确保绝不扣费，请使用没有绑定计费方式的 Free Tier 账户。

## 启动

首次使用只需运行一次设置命令。API Key 输入不会显示，也不会进入 shell history；macOS 优先保存在系统 Keychain，其他情况回退到权限为 `0600` 的本地文件：

```bash
free-router setup siliconflow
free-router setup groq
free-router
```

也可以不带 provider，按提示选择：

```bash
free-router setup
```

Docker、CI 或习惯使用环境变量的场景仍然支持原有方式：

```bash
export GROQ_API_KEY=gsk_xxx
export GEMINI_API_KEY=xxx
export GITHUB_TOKEN=github_pat_xxx

go run .
```

优先级是：环境变量 > 已保存凭据。Cloudflare 除 API Token 外仍需设置非敏感的 `CLOUDFLARE_ACCOUNT_ID`。

也可以一键安装到 `GOBIN`；未设置 `GOBIN` 时默认安装到 `$(go env GOPATH)/bin`：

```bash
make install
free-router version
free-router
```

服务默认监听 `http://localhost:1314`。不带子命令等同于 `free-router serve`。

```bash
curl http://localhost:1314/healthz
curl http://localhost:1314/v1/models

curl http://localhost:1314/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "auto",
    "messages": [{"role": "user", "content": "hello"}],
    "stream": true
  }'
```

OpenAI 客户端配置：

```text
OPENAI_BASE_URL=http://localhost:1314/v1
OPENAI_API_KEY=任意非空字符串
模型=auto
```

## 模型命名

模型 ID 使用 `provider/upstream-model`，避免不同源的同名模型冲突：

```text
groq/openai/gpt-oss-120b
gemini/gemini-3.5-flash
github-models/openai/gpt-4.1
openrouter/openai/gpt-oss-20b:free
```

指定完整 ID 时固定使用该源；`auto` 或 `free` 才会跨源 fallback。响应头会返回实际选择：

```text
X-Free-Router-Provider: groq
X-Free-Router-Model: openai/gpt-oss-120b
```

## 模型元数据

`GET /v1/models` 在 OpenAI 标准字段之外统一提供以下信息：

- `type`：`normal`、`embedding`、`rerank`、`audio`、`image`、`video`、`moderation`；
- `capabilities`：tool call、reasoning、vision、streaming，以及能力是否有明确元数据；
- `context_length` 和 `max_output_tokens`；
- `input_modalities`、`output_modalities`；
- `supported_parameters`、`supported_endpoints`；
- `provider`、`upstream_id`、`tier`、`free` 和可用的 pricing。

不同 provider 返回的信息完整度不同。`*_known: false` 表示上游没有提供明确数据，不等同于确定不支持。

```bash
curl -s http://localhost:1314/v1/models | jq '.data[] | select(.id != "auto") | {
  id, type, capabilities, context_length, max_output_tokens,
  input_modalities, output_modalities, tier
}'
```

## 自维护机制

1. 启动时并发请求所有已配置 provider 的 `/models`。
2. 每小时刷新；单个源失败时保留该源上一次成功的缓存，不影响其他源。
3. 模型新增或下线不需要发布新版本。
4. `auto` 每轮优先尝试不同 provider，避免在一个限流源中反复切模型。
5. 网络错误、额度耗尽、401/402/403、模型不兼容、429 和 5xx 会切换下一个源。
6. 带 `tools` 的请求优先使用声明支持工具调用的模型；缺少能力元数据时尝试调用并以实际响应为准。

## 接入任意免费源

任何 OpenAI 兼容服务都可以通过 `FREE_ROUTER_PROVIDERS` 加入，不需要修改代码。建议只引用密钥环境变量：

```bash
export MY_FREE_API_KEY=xxx
export FREE_ROUTER_PROVIDERS='[
  {
    "id": "my-free-provider",
    "base_url": "https://api.example.com/v1",
    "api_key_env": "MY_FREE_API_KEY",
    "tier": "free-tier"
  }
]'
```

模型目录和聊天地址不符合标准路径时，可以设置 `models_url`、`chat_url`、`auth_header`、`auth_prefix` 和 `headers`。无鉴权的本地服务使用 `"no_auth": true`。

如果源的 `/models` 提供 OpenRouter 风格的 pricing，可设置 `"filter": "zero-price"`，只允许价格为零的模型；其他源默认信任用户明确配置的免费账户。

## 命令

```bash
free-router                       # 启动服务
free-router serve --addr :9000
free-router providers             # 查看内置源及配置状态
free-router models                # 聚合所有已配置源的实时模型
free-router setup groq             # 交互式保存 API Key
free-router auth add gemini        # 添加或替换凭据
free-router auth list              # 只显示 provider 和存储后端
free-router auth remove gemini     # 删除凭据
free-router version
```

凭据文件路径遵循操作系统的用户配置目录，可用 `--credentials` 或 `FREE_ROUTER_CREDENTIALS` 覆盖。程序不会自动注册第三方账号、读取邮箱验证码或绕过 CAPTCHA；账户申请仍由用户在 provider 官方网站完成一次。

完整参数见 `free-router --help`。

## 开发与测试

```bash
make help          # 查看全部命令
make build         # 构建 bin/free-router
make test          # 单元及集成测试
make test-race     # 竞态检测
make test-cover    # 测试覆盖率
make vet           # 静态检查
make fmt           # 格式化源码
make check         # CI 必须通过的格式、vet、测试检查
make clean         # 清理构建产物
make uninstall     # 删除 make install 安装的二进制
```

GitHub Actions 会在 push 到 `main` 和 Pull Request 时自动执行 `make check`、竞态测试和构建。

## 自动发布

项目版本只有一个来源：[VERSION](./VERSION)，初始版本为 `0.1.0`。发布新版本时只需修改这个文件并推送到 `main`：

```bash
echo 0.1.1 > VERSION
git add VERSION
git commit -m 'chore: release 0.1.1'
git push
```

Release 工作流会检查 `v0.1.1` 是否已经存在：

- 已存在：跳过，不重复构建；
- 不存在：先运行完整检查和竞态测试，再跨平台构建、生成 SHA-256 校验文件、创建 Git tag 和 GitHub Release；
- 构建失败：不会创建不完整的 Release，下一次推送 `main` 时会自动重试。

Release 提供以下下载产物：

```text
Linux   amd64 / arm64
macOS   amd64 / arm64
Windows amd64 / arm64
checksums.txt
```

也可以在 GitHub Actions 页面手动运行 `Release` 工作流，补建当前 `VERSION` 对应的 Release。

项目借鉴 [OmniRoute](https://github.com/diegosouzapw/OmniRoute) 的动态目录和 fallback 思路，但刻意不包含 UI、数据库、账号池和付费路由。
