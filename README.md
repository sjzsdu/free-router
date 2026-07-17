# free-router

一个极简、OpenAI 兼容的多免费源模型路由器：自动发现并缓存模型，对外提供稳定的能力名称，跨 provider 按可配置顺序故障切换。

## 支持的免费源

程序会根据已保存凭据或环境变量自动启用 provider，没有配置密钥的源不会访问。

| Provider | 环境变量 | 免费形式 | 注册 / 获取 Key |
| --- | --- | --- | --- |
| OpenRouter | `OPENROUTER_API_KEY` | 严格选择输入、输出价格均为 0 的模型 | [OpenRouter Keys](https://openrouter.ai/keys) |
| Groq | `GROQ_API_KEY` | Free Plan | [Groq Console](https://console.groq.com/keys) |
| Cerebras | `CEREBRAS_API_KEY` | Free Tier | [Cerebras Cloud](https://cloud.cerebras.ai/) |
| Google Gemini | `GEMINI_API_KEY` | Free Tier | [Google AI Studio](https://aistudio.google.com/apikey) |
| GitHub Models | `GITHUB_TOKEN` | 免费原型额度 | [创建 PAT](https://github.com/settings/personal-access-tokens/new)（需 `models:read`） |
| Pollinations | `POLLINATIONS_API_KEY` | 免费 credits / Pollen | [Pollinations](https://enter.pollinations.ai/) |
| Hugging Face | `HF_TOKEN` | 每月免费 credits | [Access Tokens](https://huggingface.co/settings/tokens) |
| NVIDIA NIM | `NVIDIA_API_KEY` | 免费 credits | [NVIDIA API Keys](https://build.nvidia.com/settings/api-keys) |
| Mistral | `MISTRAL_API_KEY` | Experiment Plan | [Mistral Console](https://console.mistral.ai/api-keys) |
| SambaNova | `SAMBANOVA_API_KEY` | Free Tier | [SambaNova Cloud](https://cloud.sambanova.ai/apis) |
| Ollama Cloud | `OLLAMA_API_KEY` | Free Tier | [Ollama Keys](https://ollama.com/settings/keys) |
| ModelScope | `MODELSCOPE_API_KEY` | 免费推理额度 | [访问令牌](https://modelscope.cn/my/myaccesstoken) |
| SiliconFlow | `SILICONFLOW_API_KEY` | 官方标记免费的聊天模型 | [API 密钥](https://cloud.siliconflow.cn/account/ak) |
| Z.AI | `ZAI_API_KEY` | GLM Flash 免费模型 | [API Keys](https://z.ai/manage-apikey/apikey-list) |
| Cloudflare Workers AI | `CLOUDFLARE_API_TOKEN` + `CLOUDFLARE_ACCOUNT_ID` | 每日 10,000 Neurons | [创建 API Token](https://dash.cloudflare.com/profile/api-tokens) |

这些服务的条款、区域和额度会变化。除 OpenRouter 的零价模型筛选外，provider 通常不会通过 `/models` 告诉路由器账户是否启用了付费；要确保绝不扣费，请使用没有绑定计费方式的 Free Tier 账户。

OpenRouter 还支持网页中的 **OAuth 登录**：点击后在 OpenRouter 完成授权，free-router 使用 PKCE 换取用户自己的 API Key，并直接保存到本机安全凭据存储。整个过程无需复制 Key，授权回调只允许 localhost，且 10 分钟后自动失效。其他 Provider 的官网社交登录不等于 API OAuth；需要预先注册 OAuth Client 的平台暂不内置公共 Client 凭据。

## 启动

首次使用只需运行一次设置命令。API Key 输入不会显示，也不会进入 shell history；macOS 优先保存在系统 Keychain，其他情况回退到权限为 `0600` 的本地文件：

```bash
free-router setup siliconflow
free-router setup groq
free-router serve
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

go run . serve
```

优先级是：环境变量 > 已保存凭据。Cloudflare 除 API Token 外仍需设置非敏感的 `CLOUDFLARE_ACCOUNT_ID`。

也可以一键安装到 `GOBIN`；未设置 `GOBIN` 时默认安装到 `$(go env GOPATH)/bin`：

```bash
make install
free-router version
free-router serve
```

Release 压缩包中只有一个 `free-router` 二进制文件。解压后直接执行安装命令即可，不需要 Go、Make 或额外安装脚本：

```bash
chmod +x free-router
./free-router daemon install
~/.local/bin/free-router daemon status
```

程序会自行复制到 `~/.local/bin/free-router`，因此安装完成后可以删除下载目录中的原文件。整个过程无需 `sudo`。macOS 使用 LaunchAgent，Linux 使用 systemd user service；登录后自动启动，异常退出后自动恢复。安装 daemon 时，程序会把当前已命中的 Provider 环境变量保存到权限为 `0600` 的本机快照，确保后台进程也能读取；环境变量变更后重新执行 `free-router daemon install` 即可更新快照并重启。

从源码安装时也可以使用 `make daemon-install`。

日常管理命令：

```bash
free-router daemon start
free-router daemon stop
free-router daemon restart
free-router daemon logs --follow
free-router daemon uninstall
```

管理页面头部的状态按钮会显示启动方式、版本、PID、运行时间、缓存模型数和请求数，并每 5 秒检查一次连接状态。

服务默认监听 `http://localhost:1314`。`free-router serve` 用于前台运行；`free-router daemon` 管理后台守护进程。不带子命令时显示帮助，不会隐式启动服务。

启动后打开管理界面：

```text
http://localhost:1314/admin/
```

可以在网页中直接打开每个免费源的官方注册 / Key 页面，录入 API Key、测试 Provider 连接、刷新缓存、查看模型健康和请求成功率，并拖动配置每条路由的 fallback 顺序。还可以禁用单个模型，或手工覆盖模型类型、tools、vision、reasoning 能力。配置保存后立即生效；新增或删除凭据也会热加载，不需要重启。

管理界面使用 React、TypeScript、Vite、Tailwind CSS、React Query、Radix UI 和 dnd-kit 构建，生产静态资源会通过 Go Embed 打入同一个二进制。普通用户不需要安装 Node；只有修改管理界面源码时才需要运行：

```bash
make web-install
make web-build
```

```bash
curl http://localhost:1314/healthz
curl http://localhost:1314/v1/models

curl http://localhost:1314/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "chat",
    "messages": [{"role": "user", "content": "hello"}],
    "stream": true
  }'

curl http://localhost:1314/v1/embeddings \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "embedding",
    "input": ["free-router 会使用配置好的 embedding fallback 链"]
  }'

curl http://localhost:1314/v1/audio/transcriptions \
  -F 'model=audio' \
  -F 'file=@speech.wav'
```

OpenAI 客户端配置：

```text
OPENAI_BASE_URL=http://localhost:1314/v1
OPENAI_API_KEY=任意非空字符串
模型=chat
```

## 固定能力路由

客户端只需要使用以下稳定字符串，不必跟随上游模型名称变化：

| model | 能力 | OpenAI 兼容接口 |
| --- | --- | --- |
| `chat` | 普通聊天 | `/v1/chat/completions` |
| `chat-tools` | 支持 tool call 的聊天 | `/v1/chat/completions` |
| `embedding` | 向量嵌入 | `/v1/embeddings` |
| `audio` | 语音合成、转录、翻译 | `/v1/audio/speech`、`/v1/audio/transcriptions`、`/v1/audio/translations` |
| `image` | 图像生成 | `/v1/images/generations` |
| `video` | 视频生成 | `/v1/videos/generations` |
| `rerank` | 文本重排序 | `/v1/rerank` |
| `moderation` | 内容审核 | `/v1/moderations` |

`auto` 和 `free` 保持兼容：聊天请求会映射到 `chat`；请求带有 `tools` 时自动映射到 `chat-tools`。

每个能力路由都有一个有序模型列表。例如 `chat-tools`：

```text
1. groq/openai/gpt-oss-120b
2. openrouter/qwen/qwen3-coder:free
3. siliconflow/Qwen/Qwen3-8B
```

数组中的模型会严格从上到下依次尝试。它们全部不可用后，路由器会从没有出现在数组中的同路由类型健康模型里轮换选择一个作为最终兜底。列表为空时，则完全根据缓存的类型、能力和健康状态自动选择。

稳定版会跟踪每个模型的成功率、响应延迟和连续失败次数：

- 429 遵循上游 `Retry-After`，缺失时默认冷却 30 秒；
- 401/403 默认冷却 5 分钟，避免持续使用无效凭据；
- 连续网络错误或 5xx 会触发熔断；
- 冷却中的模型不会进入正常 fallback 链；如果所有候选都在冷却，会选择一个模型进行恢复探测；
- 健康统计保存在内存中，服务重启后重新统计。

## 直接指定模型

模型 ID 使用 `provider/upstream-model`，避免不同源的同名模型冲突：

```text
groq/openai/gpt-oss-120b
gemini/gemini-3.5-flash
github-models/openai/gpt-4.1
openrouter/openai/gpt-oss-20b:free
```

指定完整 ID 时固定使用该源且不 fallback。使用固定能力名称时按照管理界面中的顺序 fallback。响应头会返回实际选择：

```text
X-Free-Router-Provider: groq
X-Free-Router-Model: openai/gpt-oss-120b
```

## 模型元数据

`GET /v1/models` 在 OpenAI 标准字段之外统一提供以下信息：

- `type`：`normal`、`embedding`、`rerank`、`audio`、`image`、`video`、`moderation`；
- `route_types`：用户可配置的统一路由类型；聊天模型至少包含 `chat`，支持或可能支持 tools 时还包含 `chat-tools`；
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

## 配置文件

首次启动会在操作系统用户配置目录生成 `free-router/config.json`。API Key 不在这个文件中，仍单独存储在 Keychain 或安全凭据文件。

推荐首次运行 `free-router onboard`。它会读取当前二进制中的默认配置，生成带 `_comment`、`_help` 和每条路由说明的标准 JSON 文件，同时展开全部内置 Provider 环境变量映射。说明字段不会参与运行；已有文件默认不会被覆盖。

```bash
free-router onboard                    # 写入默认配置路径
free-router onboard ./config.json      # 写入指定路径
free-router onboard --stdout           # 只打印，不写文件
free-router onboard --force            # 明确覆盖已有配置
```

```json
{
  "version": 4,
  "provider_env": {
    "gemini": ["GEMINI_API_KEY", "MY_GEMINI_KEY"],
    "groq": ["GROQ_API_KEY"]
  },
  "routes": {
    "chat": {
      "type": "chat",
      "models": [
        "groq/openai/gpt-oss-120b",
        "siliconflow/Qwen/Qwen3-8B"
      ]
    },
    "chat-tools": {
      "type": "chat-tools",
      "require_tool": true,
      "models": []
    },
    "embedding": {
      "type": "embedding",
      "models": []
    }
  },
  "models": {
    "provider/model-with-wrong-metadata": {
      "type": "chat",
      "tool_call": true
    },
    "provider/model-to-disable": {
      "disabled": true
    }
  }
}
```

`provider_env` 是 provider 到 API Key 环境变量名数组的映射。数组按顺序查找，第一个非空环境变量会作为该 Provider 的凭据并自动启用它；用户配置的名称会排在内置名称前面，合并后自动去重。配置文件只保存变量名，不保存变量值。Cloudflare 仍需额外提供 `CLOUDFLARE_ACCOUNT_ID`。

旧版配置会自动迁移到 version 4：原来的 `normal` 聊天类型会转换成 `chat` 或 `chat-tools`，并立即写回配置文件。缺少的内置路由会自动补全。推荐通过 Web 界面修改；也可以停止服务后手工编辑。路径可用 `--config` 或 `FREE_ROUTER_CONFIG` 覆盖。

## 模型缓存与自维护

1. 首次启动并发请求所有已配置 provider 的 `/models`，生成本地缓存。
2. 后续启动优先加载缓存，服务立即可用，然后在后台刷新，不阻塞启动。
3. 普通推理请求只读取内存目录，绝不会为了选择模型临时请求 provider 的 `/models`。
4. 默认每小时刷新；单个源失败时保留该源上一次成功的缓存，不影响其他源。
5. 模型新增或下线不需要发布新版本；也可以在 Web 界面手动刷新。
6. 缓存保留类型、tool call、vision、context、输入输出模态等字段，用于能力匹配。

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

模型目录和聊天地址不符合标准路径时，可以设置 `models_url`、`chat_url`、`auth_header`、`auth_prefix` 和 `headers`。其他能力接口不符合标准路径时，可通过 `endpoints` 按路径覆盖。无鉴权的本地服务使用 `"no_auth": true`。

```json
{
  "id": "custom-free",
  "base_url": "https://api.example.com/v1",
  "api_key_env": "MY_FREE_API_KEY",
  "endpoints": {
    "/embeddings": "https://embed.example.com/run",
    "/audio/transcriptions": "https://audio.example.com/transcribe"
  }
}
```

如果源的 `/models` 提供 OpenRouter 风格的 pricing，可设置 `"filter": "zero-price"`，只允许价格为零的模型；其他源默认信任用户明确配置的免费账户。

## 命令

```bash
free-router                       # 显示帮助
free-router onboard               # 生成带说明的完整默认配置
free-router serve --addr :9000    # 前台启动服务
free-router daemon install        # 安装并启动守护进程
free-router daemon status         # 查看守护进程状态
free-router providers             # 查看内置源及配置状态
free-router models                # 聚合所有已配置源的实时模型
free-router setup groq             # 交互式保存 API Key
free-router auth add gemini        # 添加或替换凭据
free-router auth list              # 只显示 provider 和存储后端
free-router auth remove gemini     # 删除凭据
free-router version
```

管理界面默认只接受本机访问。远程管理必须同时开启远程访问并设置管理令牌：

```bash
export FREE_ROUTER_ADMIN_ALLOW_REMOTE=true
export FREE_ROUTER_ADMIN_TOKEN='使用足够长的随机字符串'
free-router serve
```

浏览器会显示 HTTP Basic 登录框，用户名固定为 `admin`，密码是令牌。管理 API 也接受 `Authorization: Bearer <token>`。服务会拒绝没有令牌的远程管理模式，并对写操作执行同源检查；生产环境仍建议放在 HTTPS 反向代理或 VPN 后面。

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

项目版本只有一个来源：[VERSION](./VERSION)，当前稳定版为 `0.1.0`。发布新版本时只需修改这个文件并推送到 `main`：

```bash
echo 0.2.1 > VERSION
git add VERSION
git commit -m 'chore: release 0.2.1'
git push
```

Release 工作流会检查 `v0.2.1` 是否已经存在：

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

项目借鉴 [OmniRoute](https://github.com/diegosouzapw/OmniRoute) 的动态目录和 fallback 思路，但保持单二进制、本地配置，不引入数据库、账号池和付费路由。
