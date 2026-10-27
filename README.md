# free-router

一个极简、OpenAI 兼容的多免费源模型路由器：由维护者核实并更新免费模型清单，对外提供稳定的能力名称，跨 provider 按可配置顺序故障切换。

## 支持的免费源

程序会根据已保存凭据或环境变量自动启用 provider，没有配置密钥的源不会访问。

| Provider | 环境变量 | 免费形式 | 注册 / 获取 Key |
| --- | --- | --- | --- |
| OpenRouter | `OPENROUTER_API_KEY` | 严格选择输入、输出价格均为 0 的模型 | [OpenRouter Keys](https://openrouter.ai/keys) |
| Groq | `GROQ_API_KEY` | Free Plan | [Groq Console](https://console.groq.com/keys) |
| Google Gemini | `GEMINI_API_KEY` | Free Tier | [Google AI Studio](https://aistudio.google.com/apikey) |
| Cohere | `COHERE_API_KEY` | 试用 Key 每月 1000 次调用 | [Cohere API Keys](https://dashboard.cohere.com/api-keys) |
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
| 科大讯飞星火 | `XFYUN_API_KEY` | Spark Lite 免费模型 | [星火 API](https://console.xfyun.cn/services/spark) |
| 智谱开放平台 | `BIGMODEL_API_KEY` | 官方 Flash 免费模型系列 | [API Keys](https://bigmodel.cn/usercenter/proj-mgmt/apikeys) |
| SiliconFlow | `SILICONFLOW_API_KEY` | 官方标记免费的聊天模型 | [API 密钥](https://cloud.siliconflow.cn/account/ak) |
| Z.AI | `ZAI_API_KEY` | GLM Flash 免费模型 | [API Keys](https://z.ai/manage-apikey/apikey-list) |
| 快手万擎 | `WQ_API_KEY` | KAT-Coder-Air-V1 免费 | [万擎控制台](https://console.streamlake.com/console/home/index) |
| Cloudflare Workers AI | `CLOUDFLARE_API_TOKEN` + `CLOUDFLARE_ACCOUNT_ID` | 每日 10,000 Neurons | [创建 API Token](https://dash.cloudflare.com/profile/api-tokens) |

这些服务的条款、区域和额度会变化。除 OpenRouter 的零价模型筛选外，provider 通常不会通过 `/models` 告诉路由器账户是否启用了付费；要确保绝不扣费，请使用没有绑定计费方式的 Free Tier 账户。

### 免费源验证与可信度边界

内置 `free-models/`（按 Provider 拆分的 `<id>.json` 与 `meta.json`）是带来源链接和生成时间的维护清单，不等同于当前账户的实时可调用证明。运行时只有同时满足以下条件的模型才进入稳定能力路由：清单仍包含该模型、对应 Provider 已配置、该“模型 + 能力”最近一次探测成功且未被健康检查隔离。

维护者可用隐藏命令生成可复现的目录发现证据：

```bash
go run . discover-model-data openrouter | jq
go run . discover-model-data all | jq
```

输出字段必须按以下语义解读：

- `checked_providers`：本次确实成功读取官方模型目录，不能据此推断真实推理一定成功；
- `skipped_providers`：未发起请求，并明确区分 `missing-credentials` 与 `agent-maintained`，前者还列出缺少的环境变量；
- `fetch_failures[].category`：把 `authentication`、`quota`、`rate-limit`、`unavailable`、`network`、`timeout`、`upstream`、`invalid-response` 等限制分开；失败源不会进入 `checked_providers`；
- `available`：仅表示本次目录发现是否成功，不代表额度充足、区域可用或模型能力已经探测成功。

模型缓存由清单指纹约束，并要求与“当前清单减去有证据的隔离项”完全一致；无隔离记录却缺模型的部分缓存会在启动时重建，避免静默缩短 fallback 池。固定路由严格保留用户排序，`round-robin` 只旋转健康候选的起点；自动路由按 Provider 交错候选，优先跨源分散限流风险。幂等请求遇到网络错误、429、401/402/403、模型 404/410 或上游 5xx 时可切换到下一候选；图片/视频生成、TTS 等非幂等请求不会在可能已被上游接收后自动重放。

### 中国大陆免费源的筛选原则

内置中国大陆来源目前包括 ModelScope、SiliconFlow、智谱开放平台、科大讯飞星火、快手万擎、Xiaomi MiMo、阿里云百炼和火山方舟。免费模型名单和判定证据统一维护在版本化数据清单中，不再写死在 Go 代码里；真实调用结果属于本机运行诊断，不会反向污染官方模型源数据。

`gift-credits`、`new-user-free-quota`、`free-trial-quota` 都属于额度型来源，并非永久免费。只有用户配置对应 Key 后它们才会启用，管理页面会持续显示计费警告。百炼用户应先开启“免费额度用完即停”；其他额度型平台应关闭后付费或确保账户没有可扣余额。Kimi 当前按输入/输出计费，MiniMax 当前使用付费 Token Plan、Credits 或按量计费，因此没有作为免费来源内置。Cerebras 官方已明确不提供永久免费层（Free Trial 为 $5/30 天且需绑定支付方式），百川智能赠送金政策（2024 年 5 月起 3 个月有效）已过期且当前全部按量计费，百度千帆官方目录已无零价模型，故三者均已从内置免费源中清理。

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

容器默认监听 `0.0.0.0:1314`，数据文件保存在 `/data`（Docker volume），端口、上游 provider 配置和密钥都通过环境变量传入。最小 `docker run` 示例：

```bash
docker run --rm --name free-router \
  -p 127.0.0.1:1314:1314 \
  -v free-router-data:/data \
  -e FREE_ROUTER_API_TOKEN='替换为推理接口随机 Token' \
  -e FREE_ROUTER_ADMIN_ALLOW_REMOTE=true \
  -e FREE_ROUTER_ADMIN_TOKEN='替换为管理界面随机 Token' \
  -e GROQ_API_KEY='可选的上游 Key' \
  docker.io/your-dockerhub-user/free-router:latest
```

要通过 Traefik labels 部署，可直接复制 `deploy/docker-compose.traefik.yml`。先创建外部网络并设置必需变量：

```bash
docker network create traefik
export DOCKERHUB_USERNAME=your-dockerhub-user
export TRAEFIK_HOST=router.example.com
export FREE_ROUTER_API_TOKEN='替换为推理接口随机 Token'
export FREE_ROUTER_ADMIN_TOKEN='替换为管理界面随机 Token'
docker compose -f deploy/docker-compose.traefik.yml up -d
```

Compose 文件将 `FREE_ROUTER_ADDR` 设为 `0.0.0.0:<端口>`，并把 Traefik 服务端口 label 指向同一容器端口。

本地构建并运行镜像时，必须显式提供推理 API Token；需要从宿主机访问管理页时，还要单独启用远程 Admin 并设置独立 Token：

```bash
docker build -t free-router .
docker run --rm -p 127.0.0.1:1314:1314 \
  -v free-router-data:/data \
  -e FREE_ROUTER_API_TOKEN='替换为推理接口随机 Token' \
  -e FREE_ROUTER_ADMIN_ALLOW_REMOTE=true \
  -e FREE_ROUTER_ADMIN_TOKEN='替换为管理界面随机 Token' \
  free-router
```

推理请求使用 `Authorization: Bearer ...`；管理页使用用户名 `admin` 和独立的 Admin Token。端口发布到宿主机 loopback 可避免管理面直接暴露到局域网。

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

程序会自行复制到 `~/.local/bin/free-router`，因此安装完成后可以删除下载目录中的原文件。整个过程无需 `sudo`。macOS 使用两个 LaunchAgent：一个运行路由服务，另一个显示原生顶部菜单栏图标；Linux 使用 systemd user service。它们会在登录后自动启动，服务异常退出后自动恢复。安装 daemon 时，程序会把当前已命中的 Provider 环境变量保存到 `~/.free-router/daemon-env.json`（权限 `0600`），确保后台进程也能读取；环境变量变更后重新执行 `free-router daemon install` 即可更新快照并重启。

macOS 菜单栏会持续显示服务状态，并提供“Open Console”“Restart Service”和“Quit Menu Bar”。退出菜单栏不会停止路由服务；重新执行 `free-router daemon start` 或 `free-router daemon install` 可再次显示。

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

可以在网页中直接打开每个免费源的官方注册 / Key 页面，录入 API Key、测试 Provider 连接、查看清单模型的健康状态和请求成功率，并拖动配置每条路由的 fallback 顺序。还可以禁用单个模型，或手工覆盖模型的多功能集合、tools、vision、reasoning 能力。配置保存后立即生效；新增或删除凭据也会热加载，运行时不会自动导入 Provider 的 `/models`。

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

每条路由可选择三种策略：

- `ordered`（按顺序）：每次从数组中第一个健康模型开始，失败后严格向下 fallback；
- `round-robin`（雨露均沾）：每次轮换一个健康模型作为首选，失败后继续沿数组 fallback，让免费额度和限流压力分散到多个来源；
- `best-quality`（高分优先）：按清单里维护的 `quality.score` 从高到低尝试。**没有评分的模型不会被当成 0 分丢弃**，而是保持原有相对顺序排在已评分模型之后，仍然留在候选池里兜底。评分缺失、过期或只覆盖部分模型时，策略只会重排候选、不会缩小候选池。

三种策略下，数组中的模型全部不可用后，路由器都会从没有出现在数组中的同路由类型健康模型里轮换选择一个作为最终兜底。列表为空时，则完全根据缓存的类型、能力和健康状态自动选择。

### 模型评分（`quality`）

评分是**清单数据**，不是运行时判断：每个模型可以在 `free-models/<provider>.json` 里带一条可追溯的记录。

```json
{
  "id": "openai/gpt-oss-120b",
  "functions": ["chat", "chat-tools"],
  "quality": {
    "source": "lmarena",
    "score": 82.5,
    "rank": 4,
    "observed_at": "2026-10-01",
    "url": "https://example.com/leaderboard"
  }
}
```

| 字段 | 说明 |
| --- | --- |
| `source` | 评分来源标识（必填）。没有来源的分数无法核实，`validate-model-data` 会直接拒绝 |
| `score` | 归一化到 0–100 的分数，越大越好；必须在 `(0, 100]` 区间内 |
| `rank` | 可选，同一来源内的排名，分数相同时用它决定先后 |
| `observed_at` | 可选，评分观测时间。榜单会变，评分不该被当成永久事实 |
| `url` | 可选，来源链接，与 `source_urls` 同一条证据纪律 |

关于可比性：不同榜单的量纲不同（Elo、指数分、胜率），`score` 只是各自归一化后的结果，**严格可比的范围是同一个 `source`**。跨来源混排是启发式，介意的场景请让同一路由里的评分来自同一来源。

评分字段是可选、可缺、可人工覆盖的，并且：

- `discover-model-data` 从不产生评分，但会**保留清单中已有的 `quality`**，避免一次目录刷新把评分洗掉；
- 评分变化会改变模型指纹，从而让旧的探测证据失效并重建缓存，不会出现“评分变了但路由还用旧健康结论”；
- Admin 的模型与路由页面会展示评分和来源，`GET /admin/api/state` 的每个模型也带 `quality` 字段。

没有任何评分时，`best-quality` 会退化成 `ordered` 的行为（全部按原顺序）。

稳定版会按“真实模型 + 固定能力”跟踪成功率、响应延迟、HTTP 状态和最近一次错误：

- 自动路由遇到网络错误、429、鉴权/额度错误、模型不兼容或上游 5xx 时，立即 fallback 到下一个候选；
- 失败能力会被标记为 `failed` 并退出对应能力路由，不会影响同一模型的其他健康能力；
- Admin 模型页可筛选“故障（已隔离）”，查看最近状态和错误原因；
- 排查完成后可点击“重新加入自动路由”；直接指定完整模型 ID 调用成功也会恢复其健康状态；
- 健康统计保存在内存中，服务重启后重新统计。

进入 Admin 的模型页时，系统会后台检测状态未知或检测缓存已超过 24 小时的全部“模型 + 能力”组合。文本能力使用 1 token 的最短请求；图片、音频、视频理解分别使用内嵌的 8×8 PNG、0.1 秒 WAV 和极小 MP4；生成能力使用最小真实任务。同一 Provider 串行、不同 Provider 全局最多并发 8 个，普通能力单次超时 10 秒，图片/视频生成最长等待 2 分钟。测试素材通过 Go Embed 打入同一个二进制，用户不需要额外维护资源文件。

检测结果缓存 24 小时；点击“重新检测全部”会在确认后忽略缓存强制重检，图片/视频生成任务可能消耗少量免费额度。Video 接口返回任务已受理即视为探测成功，不等待完整成片。任一能力探测失败都会把整个模型从运行缓存删除；诊断记录仍保留在异常列表中。只有内置免费模型清单发生变化时，本地缓存才会重建。

### 模型心跳保活

使用过程中，上游的临时故障或限流会给模型打上隔离标记，过去只能人工点「检测全部」才能恢复。心跳把同一个探测动作变成定时驱动：

- 复用「检测全部」的探测实现（同一套 capability 探针、同一 Provider 串行、同一份验证与健康记录），心跳只负责决定“什么时候测谁”，不另写一套探活代码；
- 两条独立时钟：`recovery_minutes` 重试已隔离/冷却中的模型，`interval_minutes` 复检已健康的模型，任一项设为 `0` 即关闭该时钟；
- 每轮最多 `max_probes_per_round` 个探测、`concurrency` 路并发，且同一 Provider 始终串行；候选按 Provider 交错选取，限流不会集中打在同一个免费源上；
- 连续失败的目标按指数退避重试（默认 10 → 20 → 40 分钟，最长 60 分钟），不会每轮都去打同一个坏模型；
- 图片/视频生成探测会真实消耗免费额度，心跳永不调度，仍需逐个手动确认；
- 手动「检测全部」正在进行时心跳自动让位，两者不会叠加请求；
- 日志里能看到每次心跳的结果：`heartbeat round started/finished` 汇总本轮探测数与健康/失败数，`heartbeat probe ok|failed` 记录模型、能力、Provider、耗时与当前退避目标数。

默认值刻意保守（30 分钟复检、10 分钟恢复、每轮 8 个探测、2 路并发），开箱即用又不会把 13+ 免费源的额度耗在保活流量上。Admin 模型页的「模型心跳保活」面板可以实时改这些值，保存后立即生效；也可以直接写在 `config.json`：

```json
{
  "heartbeat": {
    "enabled": true,
    "recovery_minutes": 10,
    "interval_minutes": 30,
    "max_probes_per_round": 8,
    "concurrency": 2
  }
}
```

环境变量优先级高于配置文件，便于临时停用：`FREE_ROUTER_HEARTBEAT_ENABLED`、`FREE_ROUTER_HEARTBEAT_RECOVERY_MINUTES`、`FREE_ROUTER_HEARTBEAT_INTERVAL_MINUTES`、`FREE_ROUTER_HEARTBEAT_MAX_PROBES_PER_ROUND`、`FREE_ROUTER_HEARTBEAT_CONCURRENCY`。

环境变量与环境合并后的结果同样受校验约束，不会出现“界面显示心跳开着、实际永不探测”的情况：无法解析的布尔值、非数字或负数会被忽略，并在日志里以 `heartbeat configuration problem` 警告一次；超出上限的并发/单轮数会被收敛到上限并告警；如果环境变量把两个时钟都设为 `0`，则退回默认的 30/10 分钟并告警。

## 直接指定模型

模型 ID 使用 `provider/upstream-model`，避免不同源的同名模型冲突：

```text
groq/openai/gpt-oss-120b
gemini/gemini-3.5-flash
github-models/openai/gpt-4.1
openrouter/openai/gpt-oss-20b:free
```

指定完整 ID 时固定使用该源且不 fallback。使用固定能力名称时按照管理界面选择的 `ordered`、`round-robin` 或 `best-quality` 策略调用，并在失败时 fallback。响应头会返回实际选择：

```text
X-Free-Router-Provider: groq
X-Free-Router-Model: openai/gpt-oss-120b
```

## 失败归因

所有响应都会带 `X-Request-Id`，同一个值也会出现在错误体的 `request_id` 里，方便把一次调用和日志对上。

免费源会以各种方式失败：密钥被吊销、当日额度用尽、临时 429、主机不可达。路由器把所有候选都试过之后，不会再把“最后一家”的错误原样转给调用方——那会让“整池免费额度用尽”看起来像调用方自己的凭据出了问题。现在按整池情况给出结论：

| 池内情况 | 状态码 | `code` |
| --- | --- | --- |
| 任一上游限流 | 429，并转发最小的 `Retry-After` | `rate_limited` |
| 只有鉴权/额度类故障（401/402/403） | 503 | `accounts_unavailable` |
| 全部超时或不可达 | 504 | `upstream_timeout` |
| 其余上游故障 | 502 | `upstream_failed` |
| 请求本身不合法（非 408/429 的非重试类 4xx） | 原样透传上游状态与文案 | `client_error` |
| 单候选 | 原样透传上游状态与文案 | 按错误类型归类 |

错误体保持 OpenAI 兼容，并额外带 `attempts`（每个试过的上游）、`skipped`（没有尝试的候选及原因）与 `elapsed_ms`（这次请求耗在整池尝试上的时间）：

```json
{
  "error": {
    "message": "all 2 configured free providers are rate limited; retry shortly",
    "type": "free_router_error",
    "code": "rate_limited",
    "request_id": "4da8cab94fac3f61",
    "route": "chat",
    "elapsed_ms": 437,
    "attempts": [
      {"provider": "groq", "model": "openai/gpt-oss-120b", "capability": "chat", "phase": "strict", "status": 429, "message": "rate limited", "retryable": true, "rate_limited": true, "retry_after_seconds": 12, "latency_ms": 118},
      {"provider": "openrouter", "model": "qwen/qwen3-coder:free", "capability": "chat", "phase": "strict", "status": 502, "message": "server error", "retryable": true, "latency_ms": 319}
    ],
    "skipped": [
      {"provider": "gemini", "model": "gemini-3.5-flash", "reason": "no acquirable in-flight slot; the model is cooling down after recent failures"}
    ]
  }
}
```

`phase` 为 `loose` 表示该次尝试来自“工具调用降级为普通对话”的兜底循环。`latency_ms` 用来区分“这家配错了”（例如 100ms 内返回的 401）和“这家很慢或被打爆”（例如 30s 才超时的 504）——两者需要完全不同的处理方式。

### failover 不会替你等上游

换源通常意味着换到另一个账号、另一份额度，因此失败 provider 的 `Retry-After` 只描述它自己：拿它来阻塞下一次尝试，只会把整整一个提示时长加到调用方身上（曾经可以叠加到 5 分钟）。现在：

- 跨 provider 的 fallback **立即尝试**；失败的那个模型已经按 `Retry-After` 进入冷却，在其提示期内不会再被选中；
- 只有同一个 provider 的兄弟模型（共享同一个 key 和配额）之间才保留短暂等待，且上限 2 秒。

### 不可修复的失败会被长时间隔离

免费池里最常见的一种“假可用”是密钥被吊销（401）或额度用尽（402）：重试永远不会成功，但只要它还留在候选里，每个请求就会先给它一次机会，白白消耗本来就不多的尝试预算（默认 3 次），并延后真正能服务的源。这两类失败现在按独立的隔离曲线处理：2 分钟起、指数退避、封顶 30 分钟。403 语义模糊（既可能是账号被停用，也可能是某个模型没有授权），因此仍走原来的短冷却，避免因为一个模型未授权而冻结同一个 key 下的其它模型。

隔离期内该 provider 的全部模型都不会被选中，`skipped` 会写明它正在冷却。修好 key 或充值后，无需等它自然恢复：在 Admin 的模型健康页执行 reset，或重新保存一次 provider 配置即可立即解除。`/metrics` 的 `concurrency` 现在按“在途请求”计数，不再随 fallback 次数单调增长。

## 日志

默认是 `info` 级别、`text` 格式、写 stderr，并且**每个推理请求输出一行访问日志**（流式与非流式都有，健康检查等控制面请求不刷屏）：

```text
time=2026-10-17T15:20:01.812+08:00 level=INFO msg=request request_id=4da8cab94fac3f61 method=POST path=/v1/chat/completions route=chat status=200 attempts=2 duration_ms=437 bytes=812 provider=groq model=openai/gpt-oss-120b capability=chat ttfb_ms=210 stream=true input_tokens=12 output_tokens=34 total_tokens=46
```

| 字段 | 含义 |
| --- | --- |
| `request_id` | 与响应头 `X-Request-Id`、错误体里的 `request_id` 是同一个值，一条 `grep` 就能把一次调用对齐 |
| `route` | 客户端请求的稳定能力名（`chat`、`chat-tools`、`embedding` 等） |
| `status` | 调用方实际收到的状态码，失败时的错误体也计入 |
| `attempts` | 本次请求一共尝试了几个上游；`1` 表示首选就成功，大于 `1` 就是发生过 fallback |
| `provider` / `model` | 最终应答的真实上游，客户端看不到，但排查时最先需要 |
| `duration_ms` / `ttfb_ms` / `bytes` | 总耗时、首字节耗时、写回的响应字节数 |
| `stream` | 是否为 SSE 流式响应 |
| `input_tokens` / `output_tokens` | 上游返回的用量；上游应当返回却没返回时是 `usage_missing=true`，不会估算 |
| `phase=loose` | 该响应来自“工具调用降级为普通对话”的兜底循环，属于正确性相关信息 |
| `code` | 请求失败时的归因码（`rate_limited`、`accounts_unavailable`、`upstream_timeout` 等），与错误体一致 |

日志级别按结果自动区分：成功是 `INFO`、4xx 是 `WARN`、5xx 是 `ERROR`。所以 `--log-level warn` 只留下有问题的请求。

两个开关：

```bash
free-router serve --log-level debug          # 输出每个候选的尝试、重试判定与 fallback 决策
free-router serve --log-format json          # 交给 jq / Loki / 日志采集
FREE_ROUTER_LOG_LEVEL=debug FREE_ROUTER_LOG_FORMAT=json free-router serve
```

`--log-level` 与 `--log-format` 对应环境变量 `FREE_ROUTER_LOG_LEVEL` / `FREE_ROUTER_LOG_FORMAT`。守护进程模式下，`free-router daemon install` 会把当前值写进 daemon 环境快照，修改后需要重新执行 `free-router daemon install` 并重启；`free-router daemon logs --follow` 查看后台日志。

写日志不改变路由行为：日志级别只影响可见性，不会因为调高到 `debug` 而让请求更慢或触发更多上游调用。

## 诊断（doctor）

「为什么今天不能用了」现在一条命令回答：

```bash
free-router doctor
```

```text
ok   route configuration: ~/.free-router/config.json is valid (version 6, revision 13, 12 routes, 0 with an explicit fallback list)
     hint: automatic routes are fine, but pinning a few models per route makes fallback order predictable
ok   provider credentials: 15 of 21 providers configured (181 maintained models): openrouter, groq, gemini, …
ok   local state: health: 417.5 KiB, …; statistics: 31.1 KiB, …
ok   running service: http://127.0.0.1:1314/healthz is up: 15 providers, 178 maintained models, 181 catalog entries
ok   recent failures: no request has exhausted the pool since startup
ok   service log: ~/.free-router/free-router.log, 77.1 KiB, updated 2026-10-17T22:23:16+08:00
```

它完全只读，不发任何模型请求，按顺序检查六件事：

| 检查 | 回答的问题 | 失败/警告时给出的下一步 |
| --- | --- | --- |
| `route configuration` | 配置文件能不能加载、有多少路由是显式 fallback | 修 JSON 或 `free-router onboard` 重新生成 |
| `provider credentials` | 哪些源已配置、清单里有多少可路由模型 | `free-router setup <provider>` |
| `local state` | 健康/统计文件是否存在、多大、多久没更新 | 异常膨胀时停止服务后清理 |
| `running service` | `/healthz` 是否可达、有多少 provider 与模型 | `free-router serve` 或 `free-router daemon start` |
| `recent failures` | 最近有没有整池失败，最新一次的归因码与 request id | 用 `--log-level debug` 复现，或在 Admin 健康页重置模型 |
| `service log` | daemon 日志在哪、多大、最后写入时间 | 日志过大时停止 daemon 后归档 |

- 有 `fail` 时退出码为 `1`，适合放进脚本或 CI；
- `--json` 输出同样的内容，便于其他工具消费；
- 服务要求 Admin Token 时，导出 `FREE_ROUTER_ADMIN_TOKEN` 才能读取失败列表，否则该项显示 `skipped` 并说明原因；
- systemd 环境写 journald，日志检查会提示文件不存在而不是误报故障。

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
├── free-models/      # 可选的外部免费模型清单目录（按 Provider 拆分的 <id>.json + meta.json）
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

可信模型目录、调用凭据和健康状态是三类独立数据：

1. `internal/provider/free-models/<provider>.json` 是维护者按 Provider 拆分、核实并提交的版本化数据文件，与 `meta.json`（清单级 `schema_version` / `generated_at`）一起通过 Go Embed 随二进制发布；每个 Provider 只有经过官方来源确认的 `models[]`，不再存在 policy 或运行时 allowlist。
2. 读取和缓存内置模型不需要配置 API Key。API Key 只负责启用对应 Provider 的实际调用能力，不参与模型发现，也不会改变目录内容。
3. 启动、保存凭据和 Admin 操作均不会请求 Provider `/models` 扩充目录。运行时代码只消费内置清单数据，Provider 连接地址和鉴权逻辑仍留在 Go 代码中。
4. `~/.free-router/models.json` 使用内置模型目录的稳定内容指纹校验缓存。普通模型调用失败只在内存中隔离对应的模型能力，不会删除目录中的整个模型；别名自动路由会避开该失败能力，直接指定完整模型 ID 调用成功后会恢复它。
5. Admin 主动探测失败才会从缓存持久清退整个模型，并记录模型关键路由元数据指纹。被清退模型只有自身元数据发生变化，或用户显式重新检测并验证成功后才会恢复；重启或仅更新描述、`generated_at` 均不会清空该隔离。
6. 可用 `--free-models FILE` 或 `FREE_ROUTER_FREE_MODELS` 临时加载外部清单（单个 JSON 文件，或包含多个 `<provider>.json` 与 `meta.json` 的目录），无需重新构建二进制。

### 维护免费模型清单

维护者直接核实并更新 `internal/provider/free-models/<provider>.json`。模型 ID、免费条件和来源必须来自 Provider 官方 API、模型目录、价格页或文档；一次网络失败不能作为删除旧模型的依据。

更新清单时，**必须确认你改动的模型是真实可调用的**——这一验证在写代码阶段就完成，而不是等上线后：

1. 准备密钥：复制模板并只填写本次要维护的 provider 对应的 Key（没有 Key 的 provider 在校验时会被自动跳过）：
   ```bash
   cp .env.example .env
   # 编辑 .env，只填本次要更新的 provider 的 Key
   ```
2. 编辑 `internal/provider/free-models/<provider>.json`，增删或调整模型。
3. 跑真实探测，确认清单里（配了 Key 的）模型都能用最小请求成功调用：
   ```bash
   make verify-free-models
   ```
   该命令对带 Key 的 provider 逐个发送最小 chat / embedding / audio 请求，任一部分失败即以非零状态退出，禁止合入。
4. 再跑清单结构校验与单元测试：
   ```bash
   make validate-free-models
   go test ./internal/provider ./internal/catalog ./cmd
   ```

业务代码无需跟随模型名单变化。若只想在本机立即使用另一份清单，可通过 `--free-models FILE` 或 `FREE_ROUTER_FREE_MODELS` 加载外部文件或目录。

## 接入任意免费源

任何 OpenAI 兼容服务都可以通过 `FREE_ROUTER_PROVIDERS` 配置连接，但模型仍必须由外部免费模型 manifest 的同名 Provider `models[]` 明确提供；仅配置连接和 API Key 不会在运行时自动导入 `/models`。建议只引用密钥环境变量：

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

免费模型由维护者核实后写入内置清单。运行时 Provider 配置不接受过滤策略，也不会根据 `/models` 或价格字段自行猜测免费模型。

## 命令

```bash
free-router                       # 显示帮助
free-router onboard               # 生成带说明的完整默认配置
free-router serve --addr :9000    # 前台启动服务
free-router serve --log-level debug   # 排查 fallback 时输出每个候选的决策
free-router daemon install        # 安装并启动守护进程
free-router daemon status         # 查看守护进程状态
free-router doctor                # 只读诊断：配置/凭据/服务/最近失败/日志
free-router providers             # 查看内置源及配置状态
free-router models                # 只输出已配置 Provider 的清单模型
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

模型可用性检测只会由管理界面的按钮手动触发，不会在打开页面、保存凭据或测试 Provider 连接时自动运行。检测结果和运行时故障状态默认持久化到 `~/.free-router/health.json`，可用 `--health` 或 `FREE_ROUTER_HEALTH` 覆盖；运行中失败的模型会立即退出后续候选，直到手动检测或重置后恢复。

每次实际发往上游模型的调用都会按“模型 + 能力”聚合调用数、成功率、输入/输出 Token、usage 覆盖率和平均延迟。失败后触发 fallback 的尝试也计入对应模型的调用质量；上游成功但未返回 `usage` 时只增加缺失计数，不会估算 Token。统计默认原子持久化到 `~/.free-router/statistics.json`，可用 `--statistics` 或 `FREE_ROUTER_STATISTICS` 覆盖，并可在管理界面的“统计”页面或 `GET /admin/api/statistics` 查询。

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

## Docker 镜像与发布

仓库根目录的 `Dockerfile` 使用前端、Go 编译和 Alpine 运行时三阶段构建，运行时以非 root 用户 `free-router` 启动，并对 `/healthz` 配置 Docker `HEALTHCHECK`。镜像支持 `linux/amd64` 与 `linux/arm64`，默认暴露 1314 端口；可用 `FREE_ROUTER_ADDR`、`FREE_ROUTER_*` 和各 provider 的环境变量覆盖监听地址、持久化路径、上游配置及密钥。

`.github/workflows/docker-publish.yml` 的行为如下：Pull Request 仅用 Buildx 构建 amd64/arm64，不推送；`main` 推送发布 `latest` 与 `sha-<commit>`；`vX.Y.Z` tag 推送 semver、`latest`（仅 main）和 sha tags。发布前需要在 GitHub 仓库手工配置 Actions secrets：`DOCKERHUB_USERNAME`（Docker Hub 用户名）和 `DOCKERHUB_TOKEN`（Docker Hub access token）。镜像仓库名为 `${DOCKERHUB_USERNAME}/free-router`。
