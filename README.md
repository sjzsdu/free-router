# free-router

一个极简、OpenAI 兼容的多免费源模型路由器：由维护 Formula 发现并验证免费模型，对外提供稳定的能力名称，跨 provider 按可配置顺序故障切换。

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
| Xiaomi MiMo | `MIMO_API_KEY` | 账号赠送体验金 | [API Keys](https://platform.xiaomimimo.com/#/console/api-keys) |
| 阿里云百炼 | `DASHSCOPE_API_KEY` | 新人模型免费额度 | [API Key](https://bailian.console.aliyun.com/?apiKey=1#/api-key) |
| 火山方舟 | `ARK_API_KEY` | 各模型免费体验额度 | [API Key](https://console.volcengine.com/ark/region:ark+cn-beijing/apiKey) |
| 百川智能 | `BAICHUAN_API_KEY` | 新用户赠送金 | [开放平台](https://platform.baichuan-ai.com/) |
| 智谱开放平台 | `BIGMODEL_API_KEY` | 官方 Flash 免费模型系列 | [API Keys](https://bigmodel.cn/usercenter/proj-mgmt/apikeys) |
| 百度千帆 | `QIANFAN_API_KEY` | 长期免费的 ERNIE Speed / Lite / Tiny | [API Key 管理](https://console.bce.baidu.com/qianfan/ais/console/apiKey) |
| SiliconFlow | `SILICONFLOW_API_KEY` | 官方标记免费的聊天模型 | [API 密钥](https://cloud.siliconflow.cn/account/ak) |
| Z.AI | `ZAI_API_KEY` | GLM Flash 免费模型 | [API Keys](https://z.ai/manage-apikey/apikey-list) |
| Cloudflare Workers AI | `CLOUDFLARE_API_TOKEN` + `CLOUDFLARE_ACCOUNT_ID` | 每日 10,000 Neurons | [创建 API Token](https://dash.cloudflare.com/profile/api-tokens) |

这些服务的条款、区域和额度会变化。除 OpenRouter 的零价模型筛选外，provider 通常不会通过 `/models` 告诉路由器账户是否启用了付费；要确保绝不扣费，请使用没有绑定计费方式的 Free Tier 账户。

### 中国大陆免费源的筛选原则

内置中国大陆来源目前包括 ModelScope、SiliconFlow、智谱开放平台、百度千帆、Xiaomi MiMo、阿里云百炼、火山方舟和百川智能。免费模型名单、能力验证结果和判定证据统一维护在版本化数据清单中，不再写死在 Go 代码里；没有经过 Formula 实际调用验证的模型不会进入运行目录。

`gift-credits`、`new-user-free-quota`、`free-trial-quota` 都属于额度型来源，并非永久免费。只有用户配置对应 Key 后它们才会启用，管理页面会持续显示计费警告。百炼用户应先开启“免费额度用完即停”；其他额度型平台应关闭后付费或确保账户没有可扣余额。Kimi 当前按输入/输出计费，MiniMax 当前使用付费 Token Plan、Credits 或按量计费，因此没有作为免费来源内置。

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

程序会自行复制到 `~/.local/bin/free-router`，因此安装完成后可以删除下载目录中的原文件。整个过程无需 `sudo`。macOS 使用 LaunchAgent，Linux 使用 systemd user service；登录后自动启动，异常退出后自动恢复。安装 daemon 时，程序会把当前已命中的 Provider 环境变量保存到 `~/.free-router/daemon-env.json`（权限 `0600`），确保后台进程也能读取；环境变量变更后重新执行 `free-router daemon install` 即可更新快照并重启。

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

可以在网页中直接打开每个免费源的官方注册 / Key 页面，录入 API Key、测试 Provider 连接、查看 Formula 准入模型的健康状态和请求成功率，并拖动配置每条路由的 fallback 顺序。还可以禁用单个模型，或手工覆盖模型的多功能集合、tools、vision、reasoning 能力。配置保存后立即生效；新增或删除凭据也会热加载，但不会绕过 Formula 自动导入 Provider 的 `/models`。

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
  -F 'model=speech-to-text' \
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
| `image-understanding` | 图片理解（图片输入、文本输出） | `/v1/chat/completions` |
| `image-generation` | 图片生成与编辑 | `/v1/images/generations`、`/v1/images/edits`、`/v1/images/variations` |
| `video-understanding` | 视频理解（视频输入、文本输出） | `/v1/chat/completions` |
| `video-generation` | 视频生成 | `/v1/videos/generations` |
| `audio-understanding` | 音频理解（音频输入、文本回答） | `/v1/chat/completions` |
| `speech-to-text` | 语音转文字、翻译 | `/v1/audio/transcriptions`、`/v1/audio/translations` |
| `text-to-speech` | 文字转语音 | `/v1/audio/speech` |
| `embedding` | 向量嵌入 | `/v1/embeddings` |
| `rerank` | 文本重排序 | `/v1/rerank` |
| `moderation` | 内容审核 | `/v1/moderations` |

`auto` 和 `free` 保持兼容：聊天请求会映射到 `chat`；请求带有 `tools` 时自动映射到 `chat-tools`。

每个能力路由都有一个有序模型列表。例如 `chat-tools`：

```text
1. groq/openai/gpt-oss-120b
2. openrouter/qwen/qwen3-coder:free
3. siliconflow/Qwen/Qwen3-8B
```

每条路由可选择两种策略：

- `ordered`（按顺序）：每次从数组中第一个健康模型开始，失败后严格向下 fallback；
- `round-robin`（雨露均沾）：每次轮换一个健康模型作为首选，失败后继续沿数组 fallback，让免费额度和限流压力分散到多个来源。

两种策略下，数组中的模型全部不可用后，路由器都会从没有出现在数组中的同路由类型健康模型里轮换选择一个作为最终兜底。列表为空时，则完全根据缓存的类型、能力和健康状态自动选择。

稳定版会按“真实模型 + 固定能力”跟踪成功率、响应延迟、HTTP 状态和最近一次错误：

- 自动路由遇到网络错误、429、鉴权/额度错误、模型不兼容或上游 5xx 时，立即 fallback 到下一个候选；
- 失败能力会被标记为 `failed` 并退出对应能力路由，不会影响同一模型的其他健康能力；
- Admin 模型页可筛选“故障（已隔离）”，查看最近状态和错误原因；
- 排查完成后可点击“重新加入自动路由”；直接指定完整模型 ID 调用成功也会恢复其健康状态；
- 健康统计保存在内存中，服务重启后重新统计。

进入 Admin 的模型页时，系统会后台检测状态未知或检测缓存已超过 24 小时的全部“模型 + 能力”组合。文本能力使用 1 token 的最短请求；图片、音频、视频理解分别使用内嵌的 8×8 PNG、0.1 秒 WAV 和极小 MP4；生成能力使用最小真实任务。同一 Provider 串行、全局最多并发 3 个，普通能力单次超时 10 秒，图片/视频生成最长等待 2 分钟。测试素材通过 Go Embed 打入同一个二进制，用户不需要额外维护资源文件。

检测结果缓存 24 小时；点击“重新检测全部”会在确认后忽略缓存强制重检，图片/视频生成任务可能消耗少量免费额度。Video 接口返回任务已受理即视为探测成功，不等待完整成片。任一能力探测失败都会把整个模型从运行缓存删除；诊断记录仍保留在异常列表中。该模型只有在新版 Formula 清单再次验证通过后才会重新进入目录。

## 直接指定模型

模型 ID 使用 `provider/upstream-model`，避免不同源的同名模型冲突：

```text
groq/openai/gpt-oss-120b
gemini/gemini-3.5-flash
github-models/openai/gpt-4.1
openrouter/openai/gpt-oss-20b:free
```

指定完整 ID 时固定使用该源且不 fallback。使用固定能力名称时按照管理界面选择的 `ordered` 或 `round-robin` 策略调用，并在失败时 fallback。响应头会返回实际选择：

```text
X-Free-Router-Provider: groq
X-Free-Router-Model: openai/gpt-oss-120b
```

## 模型元数据

`GET /v1/models` 只返回当前至少有一个健康候选的稳定能力名称，保证 Agent 不依赖具体 Provider 或真实模型；某项能力全部故障时会暂时从列表消失。完整物理模型目录只通过本机 Admin API/UI 展示，其中包括：

- `type`：上游模型的宽泛媒体分类，仅用于诊断；实际路由以 `functions` 为准；
- `functions`：模型可参与的固定能力数组；一个模型可以同时支持文本对话、图片理解和工具调用等多个能力；
- `capabilities`：tool call、reasoning、vision、streaming，以及能力是否有明确元数据；
- `context_length` 和 `max_output_tokens`；
- `input_modalities`、`output_modalities`；
- `supported_parameters`、`supported_endpoints`；
- `provider`、`upstream_id`、`tier`、`free` 和可用的 pricing。

不同 provider 返回的信息完整度不同。`*_known: false` 表示上游没有提供明确数据，不等同于确定不支持。

```bash
curl -s http://localhost:1314/v1/models | jq '.data[].id'
```

## 配置文件

free-router 的全局配置与运行资料统一存放在 `~/.free-router`：

```text
~/.free-router/
├── config.json       # 路由、Provider 环境变量映射和模型覆盖
├── credentials.json  # Keychain 不可用时的仅限当前用户凭据文件
├── free-models.json  # 可选的外部免费模型清单
├── models.json       # 自动维护的模型目录缓存
├── daemon-env.json   # 守护进程环境快照
└── free-router.log   # macOS 守护进程日志
```

这些文件按需创建。API Key 不写入 `config.json`；macOS 优先存入系统 Keychain，其他平台或 Keychain 不可用时才使用权限为 `0600` 的 `credentials.json`。

推荐首次运行 `free-router onboard`。它会读取当前二进制中的默认配置，生成带 `_comment`、`_help` 和每条路由说明的标准 JSON 文件，同时展开全部内置 Provider 环境变量映射。说明字段不会参与运行；已有文件默认不会被覆盖。

```bash
free-router onboard                    # 写入默认配置路径
free-router onboard ./config.json      # 写入指定路径
free-router onboard --stdout           # 只打印，不写文件
free-router onboard --force            # 明确覆盖已有配置
```

```json
{
  "version": 6,
  "provider_env": {
    "gemini": ["GEMINI_API_KEY", "MY_GEMINI_KEY"],
    "groq": ["GROQ_API_KEY"]
  },
  "routes": {
    "chat": {
      "capability": "chat",
      "strategy": "ordered",
      "models": [
        "groq/openai/gpt-oss-120b",
        "siliconflow/Qwen/Qwen3-8B"
      ]
    },
    "chat-tools": {
      "capability": "chat-tools",
      "require_tool": true,
      "models": []
    },
    "embedding": {
      "capability": "embedding",
      "models": []
    }
  },
  "models": {
    "provider/model-with-wrong-metadata": {
      "functions": ["chat", "chat-tools", "image-understanding"],
      "tool_call": true
    },
    "provider/model-to-disable": {
      "disabled": true
    }
  }
}
```

`provider_env` 是 provider 到 API Key 环境变量名数组的映射。数组按顺序查找，第一个非空环境变量会作为该 Provider 的凭据并自动启用它；用户配置的名称会排在内置名称前面，合并后自动去重。配置文件只保存变量名，不保存变量值。Cloudflare 仍需额外提供 `CLOUDFLARE_ACCOUNT_ID`。

旧版配置会一次性迁移到 version 6：`image`、`video` 分别迁移到生成能力；旧 `audio` 优先级复制到 `speech-to-text` 与 `text-to-speech`，随后可在 Admin 中按真实能力调整。运行时不再保留含义模糊的旧别名。缺失策略默认为 `ordered`，缺失的固定能力路由会自动补全并写回配置。推荐通过 Web 界面修改；也可以停止服务后手工编辑。

## 模型缓存与自维护

免费资格、运行缓存和健康状态是三类独立数据：

1. `internal/provider/free-models.json` 是版本化的免费资格清单，通过 Go Embed 随二进制发布；Provider 连接地址和鉴权逻辑仍留在 Go 代码中。
2. 运行时只接受清单中 `policy=inventory` 且列出具体模型的数据；启动、保存凭据和手动刷新均不会请求 Provider `/models` 扩充目录。
3. `~/.free-router/models.json` 是带有 Formula `generated_at` 的可用模型缓存。模型调用或 Admin 探测失败后会立即从该缓存删除，同一版清单下重启也不会复活。
4. 只有 Formula 发布了新 `generated_at`，缓存才会从新版 inventory 重建，被淘汰模型才有机会在重新验证后恢复。
5. 可用 `--free-models FILE` 或 `FREE_ROUTER_FREE_MODELS` 临时加载外部清单，无需重新构建二进制。

### 并发更新免费模型清单

仓库提供 [.tt/formulas/discover-free-models.toml](./.tt/formulas/discover-free-models.toml)。它为每个内置 Provider 启动独立并发调研分支，只接受官网文档、官方价格页或官方 API 作为免费证据；维护专用命令会读取已配置 Provider 的官方目录，对每个声明能力执行最小真实请求，只有至少一个能力成功的模型才写入 inventory。调研输出会先经过确定性的归一化和逐 Provider 校验，再保守合并、原子写入清单，并调用 Go 的语义校验器。

Formula 结束报告会区分 `attempted_at` 和真正发生数据变化的 `generated_at`，并列出接受/拒绝数量和被拒绝的 Provider。一次调研执行成功不等于数据已更新；只有通过证据和结构校验的变化才会推进清单时间戳。

Formula 不需要日期参数；发生有效数据变化时，会使用实际运行时刻更新 `generated_at`，并为新发现且缺少时间的模型补上 `verified_at`。

```bash
tt formula validate .tt/formulas/discover-free-models.toml
tt formula run discover-free-models --dir .tt/formulas
go run . validate-model-data internal/provider/free-models.json
make test-formula
```

维护者定期运行 Formula、复核 diff 后提交数据文件即可；业务代码无需跟着模型名单变化。若只想在本机立即使用生成结果，可把清单复制到 `~/.free-router/free-models.json`，并设置 `FREE_ROUTER_FREE_MODELS` 后重启服务。

## 接入任意免费源

任何 OpenAI 兼容服务都可以通过 `FREE_ROUTER_PROVIDERS` 配置连接，但运行模型仍必须由外部 Formula manifest 明确提供 inventory；仅配置连接和 API Key 不会在运行时自动导入 `/models`。建议只引用密钥环境变量：

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
free-router models                # 输出 Formula 已准入的本地模型
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

凭据回退文件默认为 `~/.free-router/credentials.json`，可用 `--credentials` 或 `FREE_ROUTER_CREDENTIALS` 覆盖。程序不会自动注册第三方账号、读取邮箱验证码或绕过 CAPTCHA；账户申请仍由用户在 provider 官方网站完成一次。

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
