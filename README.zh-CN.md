# opencode2api

[English](README.md) | [简体中文](README.zh-CN.md) | [Tiếng Việt](README.vi.md) | [Italiano](README.it.md)

使用 Go 编写的 OpenCode Zen / Zen Go 网关。客户端调用 OpenAI Chat Completions、OpenAI Responses 或 Anthropic Messages，网关选择可用上游，并在原生协议不同时转换请求和响应。System One 决策使用独立透传接口。

管理 WebUI 内嵌于可执行文件，运行时无需 Node.js 或数据库。模型发现、推理、OAuth 和可选 WARP 仍需访问相应上游服务。

## 赞助商

<table>
  <tr>
    <td width="190" align="center">
      <a href="https://www.ipwo.net/?ref=githubopencode2api">
        <img src="assets/IPWO.png" width="163" alt="IPWO">
      </a>
    </td>
    <td>
      <a href="https://www.ipwo.net/?ref=githubopencode2api">IPWO</a> 住宅代理为 AI 开发与 API 工作流提供灵活的网络连接支持，覆盖 195+ 个国家和地区，支持 HTTP(S) 与 SOCKS5 协议。<br>
      适用于跨地区网络测试、合规数据采集及自动化开发场景。提供动态代理、静态代理及不限量代理，支持免费测试和地区选择。优惠折扣码：<b>0200</b>。
    </td>
  </tr>
</table>

## 快速开始

从 [GitHub Releases](https://github.com/jasonxu114514/opencode2api/releases) 下载可执行文件，或使用 **Go 1.24 及以上版本**编译：

```bash
git clone https://github.com/jasonxu114514/opencode2api.git
cd opencode2api
cp config.example.json config.json
go build -o opencode2api ./cmd/opencode2api
```

启动前编辑 `config.json`：

1. 将 `server_keys` 替换为自己的本地 API Key。
2. 在 `zen_keys` 填入 Zen API Key（推荐），或在 `go_keys` 填入 Go Key。示例关闭匿名通道，且两个上游 Key 数组为空，必须先配置上游凭证才能启动；已保存的 Go OAuth（`go_oauth`）或 Zen OAuth（`zen_oauth`）连接也满足此要求。不支持在凭证全空时先启动服务、再进入 WebUI 完成首次 OAuth 登录。
3. 替换 `webui.password`。示例配置启用了 WebUI，用户名为 `admin`。

**匿名通道可能已被上游重点封堵，优先使用 Zen API Key。** 配置与 WebUI 默认关闭匿名访问；已有配置中明确设置的 `anonymous: true` 不会被自动改为关闭。只有接受免费请求仍可能失败时，才主动开启。

```bash
./opencode2api -config config.json
```

Windows 下使用 `Copy-Item config.example.json config.json` 复制配置，执行 `go build -o opencode2api.exe ./cmd/opencode2api` 编译，再运行 `.\opencode2api.exe -config config.json`。

示例配置的 API 监听 `127.0.0.1:8080`，WebUI 监听 `0.0.0.0:8081`。本机可访问 `http://localhost:8081`。通过网络访问管理界面时，请限制访问范围，并使用 HTTPS 反向代理。

命令行参数：

| 参数          | 默认值        | 用途                  |
| ------------- | ------------- | --------------------- |
| `-config`     | `config.json` | 配置文件路径。        |
| `-listen`     | 未设置        | 覆盖 API 监听地址。   |
| `-web-listen` | 未设置        | 覆盖 WebUI 监听地址。 |

配置所在目录需要可写，以便迁移密码、保存配置和写入模型缓存。

## Docker 与 Compose

已发布镜像为 `ghcr.io/jasonxu114514/opencode2api`。先按前文准备 `config.json`。需要通过发布端口访问时，容器内部 API/WebUI 应监听 `0.0.0.0:8080`、`0.0.0.0:8081`；访问范围由宿主机端口绑定及必要的 HTTPS 入口限制。

```bash
docker compose up -d
docker compose logs -f
```

Compose **仅在首次启动**从宿主配置初始化 `opencode2api-state` 卷。此后实际配置位于卷中，建议通过 WebUI 修改。需重新导入宿主文件时：

```bash
docker compose cp config.json opencode2api:/var/lib/opencode2api/config.json
docker compose restart
```

覆盖前先备份实际配置与状态。入口脚本以普通用户运行服务，仅在初始化时把示例 API 的准确值 `127.0.0.1:8080` 改为 `0.0.0.0:8080`。Compose 使用只读根文件系统、可写状态卷与 `/tmp` tmpfs。发布端口默认绑定宿主全部接口；仅本机访问时，将 `ports` 改为 `127.0.0.1:8080:8080`、`127.0.0.1:8081:8081`。

| Compose 变量 | 默认值 | 作用 |
| --- | --- | --- |
| `OPENCODE2API_VERSION` | `latest` | 镜像标签；固定版本时指定发行标签。 |
| `OPENCODE2API_PORT` | `8080` | 宿主 API 端口。 |
| `OPENCODE2API_WEBUI_PORT` | `8081` | 宿主管理端口。 |
| `OPENCODE2API_LISTEN` | 未设置 | 显式覆盖容器 API 监听。 |
| `OPENCODE2API_WEBUI_LISTEN` | 未设置 | 显式覆盖容器管理监听。 |

后两项在容器内为 `LISTEN_ADDRESS`、`WEBUI_LISTEN_ADDRESS`，为空时沿用实际配置。更改宿主端口不更改内部监听；更改内部 API 端口还须修改映射及默认检查 8080 的镜像健康检查。`proxyfile` 必须挂载到配置的容器内路径。

需要本地镜像时：

```bash
docker build -t opencode2api:local .
docker volume create opencode2api-state
docker run -d --name opencode2api \
  -p 127.0.0.1:8080:8080 -p 127.0.0.1:8081:8081 \
  -e CONFIG_SEED_PATH=/run/config/opencode2api.json \
  -v "$PWD/config.json:/run/config/opencode2api.json:ro" \
  -v opencode2api-state:/var/lib/opencode2api \
  opencode2api:local
```

WARP 需要向 Cloudflare `2408` 端口发送 UDP，不需要 host 网络或额外网络能力；网络阻断该流量时无法初始化 WARP。

## 调用 API

以下是现有兼容接口。OpenAI 兼容客户端的 Base URL 可填 `http://127.0.0.1:8080/v1`；管理接口位于另一监听端口。

| 方法 | 路径 | 行为 |
| --- | --- | --- |
| `GET` | `/v1/models` | 当前配置可路由的模型，以及符合条件的轮转别名。 |
| `POST` | `/v1/chat/completions` | Chat Completions，支持 JSON 和 SSE。 |
| `POST` | `/v1/responses` | Responses，支持 JSON 和 SSE。 |
| `POST` | `/v1/messages` | Anthropic Messages，支持 JSON 和 SSE。 |
| `POST` | `/v1/systemone` | 原生 System One 决策透传，不转换协议。 |
| `GET` | `/healthz` | 无需认证的就绪检查与资源摘要。 |

全部 `/v1/*` 接口要求通过 `Authorization: Bearer ...` 或 `x-api-key` 提交已启用的 `server_keys` 值。请求体上限为 32 MiB。推理响应的 `x-request-id` 可用于关联日志。

先查询实际可用的模型 ID：

```bash
curl http://127.0.0.1:8080/v1/models \
  -H 'Authorization: Bearer YOUR_LOCAL_API_KEY'

curl http://127.0.0.1:8080/v1/chat/completions \
  -H 'Authorization: Bearer YOUR_LOCAL_API_KEY' \
  -H 'Content-Type: application/json' \
  -d '{"model":"MODEL_ID","messages":[{"role":"user","content":"你好"}]}'
```

Responses 与 Anthropic Messages 分别使用以下标准 JSON 请求体，发送到对应接口：

```json
{"model":"MODEL_ID","input":"你好"}
```

```json
{"model":"MODEL_ID","max_tokens":512,"messages":[{"role":"user","content":"你好"}]}
```

Anthropic 客户端还可发送 `anthropic-version: 2023-06-01`。流式请求添加 `"stream": true`，用 `curl -N` 或能即时消费 SSE 的客户端。HTTP 200 后仍可能发生流错误，需检查错误事件与最终结果。

### 思考强度

两个方向的强度都会如实保留：显式的 `output_config.effort` 不会再被 `thinking`
块遮蔽，`budget_tokens` 保持其档位（`8192` 为 `high`，`32000` 为 `xhigh`，
两者可区分），发往 Responses 时强度会以 `reasoning.effort` 形式传递而不再丢失。

WebUI 的配置中心也提供默认强度和按模型覆盖。管理 API 的 `GET /api/config`
会返回 `reasoning`，`PUT /api/config` 接受同名对象；省略该字段会保留现有值，
发送空对象则会清除它。

### 代理

代理支持 `direct`、`http://`、`https://`、`socks5://` 和 `socks5h://`，URL 可包含认证信息。配置内代理先加载，再追加 `proxyfile` 内容，并按首次出现的顺序去重。

代理文件每行一个地址，允许空行和注释：

```text
# 首选代理
http://user:password@127.0.0.1:7890
socks5://127.0.0.1:1080  # 备用代理
direct
```

支持 `#`、`;`、`//` 注释标记，标记需要位于行首或空白字符之后。

#### 轮换代理池

在条目末尾追加 `#pool`（或在开头加 `pool+`）即可声明该地址是轮换代理池（例如 [Resin](https://github.com/Resinat/Resin)），每次新建 TCP 连接都会从不同出口 IP 发出：

```text
http://127.0.0.1:7890#pool
socks5://127.0.0.1:1080#pool
pool+http://127.0.0.1:7891
```

代理池条目的处理方式与静态代理完全不同：

| 行为         | 静态代理                                    | `#pool` 条目                                              |
| ------------ | ------------------------------------------- | --------------------------------------------------------- |
| 连接复用     | Keep-alive + HTTP/2 多路复用（`performance.*` 连接池参数） | 每次请求新建 TCP + CONNECT 隧道（`DisableKeepAlives`，禁用 HTTP/2） |
| 失败冷却     | 15s–120s 指数退避，并受 `Retry-After` 影响  | 无冷却：下一次请求直接换一个新出口 IP                     |
| 可用性剔除   | 标记不可用、迁移 Key、每 15 分钟复检        | 永不剔除，完全跳过健康检查                                |
| 匿名通道重试 | 每个出口最多访问一次，最多 `performance.max_anonymous_exits` 个出口 | 最多尝试 `retry.max_attempts` 次，循环该池条目            |

当 URL 背后的服务本身就在做轮换、而池内出口 IP 会被单独限流或不稳定时使用它：网关会把每次尝试都当作新出口，而不是把整个会话固定在某个出口上。只要一条 `#pool` 条目即可，无需再罗列几十个出口。静态条目仍然保留下文的连接池行为。

### 超时与连接池

| 字段                                    | 默认值 | 含义                                             |
| --------------------------------------- | ------ | ------------------------------------------------ |
| `retry.max_attempts`                    | `3`    | 每个认证 Tier 的尝试次数，包含首次请求。         |
| `retry.timeout_seconds`                 | `300`  | 推理请求总超时，包含流式响应读取。               |
| `performance.attempt_timeout_seconds`   | `0`    | 单次尝试的总超时；0 表示使用请求总超时。         |
| `performance.max_anonymous_exits`       | `8`    | 每个匿名请求最多尝试的静态代理出口数。           |
| `performance.free_tier_block_seconds`   | `900`  | 被免费层标记的出口冷却时长。                     |
| `performance.connect_timeout_seconds`   | `5`    | 建立连接的超时。                                 |
| `performance.failure_cooldown_seconds`  | `15`   | 失败冷却基数。                                   |
| `performance.max_idle_conns`            | `2048` | 每个代理 Transport 的空闲连接上限。              |
| `performance.max_idle_conns_per_host`   | `256`  | 每个 Transport 对单个主机的空闲连接上限。        |
| `performance.max_conns_per_host`        | `0`    | 单个主机的连接上限；0 表示不限制。               |
| `performance.idle_conn_timeout_seconds` | `120`  | 空闲连接保留时间。                               |
| `performance.stream_continuation`       | `false` | 启用后，匿名 SSE 流中断可换出口续写。            |
| `performance.stream_continuation_max`   | `1`    | 首次中断后允许的续写次数。                       |
| `performance.stream_continuation_tool_args` | `false` | 启用后续写未完成的工具调用参数。                |

续写使用独立的超时预算，不会重放已经完成的响应。工具参数续写会把片段接到客户端已经收到的调用上，使最终参数字符串保持有效 JSON。`performance.stream_continuation_silent_close` 可在无法完整接收时用普通结束块关闭当前轮次，而不是发送内联错误；默认关闭。

单次响应头超时不会超过请求总超时。希望慢节点仍能留出回退时间时，可以将它设为小于总超时的值。请求上下文过期后会停止后续尝试，不会惩罚尚未实际使用的 Key 或代理。

### 日志与管理端

| 字段                          | 默认值或要求                                    |
| ----------------------------- | ----------------------------------------------- |
| `logging.level`               | `info`；可选 `debug`、`info`、`warn`、`error`。 |
| `logging.ring_size`           | `2000`；范围为 100–50,000。                     |
| `logging.dump_request_bodies` | `false`；配合 `debug` 日志级别输出上游请求体。  |
| `webui.enabled`               | 省略时为 `false`；示例配置设置为 `true`。       |
| `webui.listen`                | `0.0.0.0:8081`。                                |
| `webui.username`              | 启用 WebUI 时必填；示例为 `admin`。             |
| `webui.password`              | 首次初始化密码，最小长度为 10。                 |
| `webui.password_hash`         | 自动生成的 Argon2id 哈希，用于替代明文密码。    |
| `webui.session_ttl_minutes`   | `720`；范围为 5–10,080。                        |

启动时会将初始密码哈希化，并从配置中删除明文；包含初始明文密码的备份也会删除。配置和备份仍包含运行所需的上游 Key、代理凭据，应限制文件访问权限。

请求体日志默认关闭，每份准备好的上游请求体最多输出 64 KiB。已配置的敏感值会脱敏，诊断响应也会掩盖凭证字段并清理上游错误文本，但对话内容本身仍可能包含敏感信息。

## WebUI 与诊断

管理监听端口提供 WebUI 和 `/api/*` 接口。认证使用单一管理员账号、服务端 Session、HttpOnly / SameSite Cookie、写操作 CSRF 校验及登录限速。

| 管理接口                                         | 用途                                            |
| ------------------------------------------------ | ----------------------------------------------- |
| `POST /api/auth/login`                           | 登录，获取 Session Cookie 和 CSRF Token。       |
| `GET /api/auth/session`、`POST /api/auth/logout` | 查看或结束当前 Session。                        |
| `GET /api/config`、`PUT /api/config`             | 读取脱敏配置或应用修改。                        |
| `POST /api/config/reload`                        | 从磁盘重载配置。                                |
| `POST /api/config/reveal`                        | 验证密码后查看配置中的敏感值。                  |
| `PUT /api/account`                               | 验证密码后修改账号，修改后使已有 Session 失效。 |
| `GET /api/monitor`                               | 请求、Token、上游和资源统计。                   |
| `GET /api/debug/models`                          | 模型路由、Key 指纹和 metadata 诊断。            |
| `POST /api/debug/inference`                      | 执行 Playground 请求。                          |
| `GET /api/logs`、`GET /api/logs/stream`          | 最近日志或 SSE 实时订阅。                       |

Playground 需要已登录的 Session 和 `X-CSRF-Token`，每个客户端 IP 每分钟最多 12 次：

```json
{
  "protocol": "chat",
  "key": { "mode": "auto" },
  "request": {
    "model": "MODEL_ID",
    "messages": [{ "role": "user", "content": "Hello" }]
  }
}
```

服务端强制设置 `stream: false`。需要指定 Key 时，使用 `{"mode":"selected","tier":"zen","id":"KEY_FINGERPRINT"}`，其中指纹来自 `/api/debug/models`。指定 Key 的诊断最多执行一次上游尝试，不进入匿名通道，不轮换 Key，不切换 Tier，也不重放 reasoning 请求。

**自动模式和指定 Key 模式都不会改变生产 Key 的冷却、失败次数，以及代理的健康状态和绑定关系。** 诊断仍会真实访问上游，可能消耗供应商配额，并会计入监控。

执行后，管理接口返回 HTTP 200，实际结果位于 `ok`、`http_status`、`request_id`、`route` 和 `response`。指定 Key 时还返回 `selected_key` 与 `key_test`，其值包括 `usable`、`rejected`、`rate_limited`、`transport_error`、`upstream_error`、`request_error`、`unavailable`。管理请求格式、认证、CSRF 和限速错误仍返回各自对应的 HTTP 状态。

#### System One

System One 要求选择 `native_protocol` 为 `systemone` 的模型，例如：

```json
{"model":"SYSTEM_ONE_MODEL_ID","state":"1 + 1 = 2","questions":{"arithmetic":{"type":"noul","instructions":"Is 1 + 1 equal to 2?"}}}
```

网关直接转发决策请求与答案。消息接口也能识别 System One 模型的这种决策结构，但普通聊天内容无法转换成决策问题。上游 System One 流式响应直接透传，不提供流重置恢复。

### 兼容边界

三种消息协议支持文本、可表达的图片/文件内容、reasoning、函数定义、工具调用、工具结果和流式响应。原生请求保留未被网关主动整形的供应商字段；跨协议只转换已有对应表示的内容，例如 Chat `seed` 和 JSON 输出约束不转换为 Anthropic 参数。不支持的内容或工具可能返回校验错误。

Responses 历史中的 assistant reasoning、文本和函数调用会合并为 assistant 轮次；用户消息和工具结果分隔轮次。转换到 Chat Completions 时，缺失或不匹配的工具结果仍会报错。

网关不实现 embeddings、文件上传、图片生成、Responses 检索或取消，也不替客户端保存会话历史。请求需携带所需历史；上游可能提供的有状态功能不代表网关具有本地会话存储。

## 配置与路由

从 [config.example.json](config.example.json) 开始配置。文件支持 `//` 和 `/* ... */` 注释，管理 API 请求体须使用标准 JSON。未知字段、无效值、重复顶层字段及同一 OAuth 方案的新旧字段冲突会被拒绝。保存会规范化配置并去掉注释。

| 配置项 | 默认值 / 含义 |
| --- | --- |
| `server_keys` | 必须保留至少一个启用的本地 Key。 |
| `zen_keys`、`go_keys` | 独立上游 Key 池；示例均为空。 |
| `prefer` | `go`；也可设为 `zen`，决定可用认证通道的首选顺序。 |
| `anonymous` | `false`；为符合条件的免费模型启用上游 `public` 凭证。 |
| `upstream.zen` | 手动 Zen Key 默认使用 `https://opencode.ai/zen`。 |
| `upstream.go` | 手动 Go Key 及其额度接口默认使用 `https://opencode.ai/zen/go`。 |
| `upstream.anonymous` | 匿名通道默认使用 `https://opencode.ai/inference`。 |
| `models.refresh_seconds` | `300`；模型发现间隔。 |
| `models.protocols` | 按模型覆盖原生协议：`chat`、`responses`、`anthropic`、`systemone`。 |
| `models.free_only` | `false`；发现与路由均仅允许免费模型。 |
| `models.active_probes` | `false`；可选的免费模型合成推理探测。 |
| `reasoning.effort` | 内置默认未设置；示例设为 `high`。 |
| `reasoning.effort_by_model` | 按模型覆盖默认思考强度。 |

### 模型与通道选择

模型发现结合上游目录与 `https://models.opencode.ai/api.json` 的 OpenCode 能力和价格目录，按 Tier 保存原生协议，并通过 `/v1/models` 提供已知的上下文/Token 上限、价格、`native_protocol`、`supported_endpoints` 与 reasoning 信息。价格及弃用元数据每 24 小时刷新，失败保留已有数据；价格元数据不是账单估算或账户余额。

使用手动 Key 时，明确开启匿名后，符合条件的免费模型可先尝试匿名通道，再按 `prefer` 尝试可用 Key 池；匿名关闭时使用已配置 Key。模型 ID 不区分大小写包含 `free`，或元数据表明未弃用且输入/输出价格均为零，即可判为免费，但不保证上游接受。付费模型需要适当的认证通道。`models.free_only` 同时限制轮转组成员。

OAuth 另有规则：请求选择 OAuth 路由后，只使用该 Tier 已确认的路由，不因失败改走匿名、手动 Key 或其他 Tier。Go OAuth 授权失败、额度耗尽或返回 429，均不会触发 PAYG 请求。两类通道都能服务同一模型时，明确选择 `prefer: "zen"` 允许使用已连接的 Zen PAYG 通道。

### 超时、代理与会话亲和

| 配置项 | 默认值 | 含义 |
| --- | --- | --- |
| `retry.max_attempts` | `3` | Key 重试循环预算；匿名与轮转有各自选择规则。 |
| `retry.timeout_seconds` | `300` | 整个推理请求预算，含流式消费。 |
| `retry.quota_gate_seconds` | `0` | 免费层 `Retry-After` 达到该值后开启快速失败闸门；0 表示关闭。 |
| `retry.max_retry_after_seconds` | `60` | 限制转发给客户端的免费层 `Retry-After`；0 表示不修改。 |
| `performance.attempt_timeout_seconds` | `0` | 可选的较短单次尝试上限；零沿用请求超时。 |
| `performance.first_event_timeout_seconds` | `0` | 可选的首事件等待上限；零关闭。 |
| `performance.connect_timeout_seconds` | `5` | 建立连接超时。 |
| `performance.failure_cooldown_seconds` | `15` | 失败后的基础冷却时间。 |
| `performance.max_anonymous_exits` | 有效值 `8` | 单次匿名请求的不同静态出口上限；零使用内置值。 |
| `performance.free_tier_block_seconds` | 有效值 `900` | 免费通道出口封锁冷却时间；零使用内置值。 |
| `performance.max_idle_conns` | `2048` | 每个 transport 的空闲连接上限。 |
| `performance.max_idle_conns_per_host` | `256` | 每个 host/transport 的空闲连接上限。 |
| `performance.max_conns_per_host` | `0` | 连接数上限；零表示不限。 |
| `performance.idle_conn_timeout_seconds` | `120` | 空闲连接寿命。 |

`proxies` 支持 `direct`、HTTP、HTTPS、SOCKS5、SOCKS5H URL。`proxyfile` 从配置目录相对路径读取，每个非空行一个代理，合并后去重；最终为空时使用直连。行首或空白后的 `#`、`;`、`//` 表示注释。代理 URL 中的账号密码属于敏感信息。

供应商负责轮换出口时，可写成 `pool+https://proxy.example:8443` 或 `https://proxy.example:8443#pool`。每次尝试新建连接，不复用 keep-alive/HTTP/2，也不采用静态节点的冷却和健康淘汰。实际出口轮换由供应商提供。静态代理保留连接复用、健康跟踪与 Key 绑定；`proxy_bindings` 可按 Key 指纹绑定已配置代理。

建议为独立会话发送 `x-session-id`。同时支持 OpenCode 会话头和既有 conversation 字段；未指定时使用初始 system/user 内容生成种子。网关生成的身份按本地 API Key 与进程生命周期隔离，有效的官方 OpenCode 会话 ID 原样保留。同一 Key 下相同开头可能共享亲和性，需要用户隔离时使用独立 Key。

可选 `models.anonymous_backoff` 在连续失败后暂停整个匿名通道，默认阈值 `6` 次、基础 `1` 小时、最大 `24` 小时；不配置此对象则关闭该通道级熔断。

### 免费模型整形与 reasoning

免费模型兼容逻辑会按需注入官方 OpenCode 系统提示词和工具定义，将客户端工具改名为带不透明 `mcp_` 前缀的线上名称，并在响应中还原客户端名称。v2 请求会保留调用方提供的 `prompt_cache_key`；未提供时使用生成的会话 ID，使缓存 Key 与请求亲和性一致。使用 Key 的免费模型也可能经过整形。网关只转发工具调用，不会在本机执行工具；System One 不经过消息/工具整形。

注入提示词包含目录、工作区、平台、模型与 UTC 日期，默认描述网关主机。远程客户端可配置兼容文本：

```json
{"working_directory":"C:\\work\\project","workspace_root":"C:\\work\\project","platform":"windows","is_git_repo":true}
```

将此对象合并到 `upstream.prompt_environment`。目录仅作为文本使用，不会由此访问；`windows` 规范为 `win32`，还支持 `linux`、`darwin`。客户端已提供的官方提示词不被覆盖。界面没有这些高级字段，需编辑配置并重载；任意环境覆盖值尚未经过真实上游校验。

思考强度接受 `minimal`、`low`、`medium`、`high`、`xhigh`、`max`、`none`、`auto` 或空值，客户端明确指定的强度优先。`none` 移除 reasoning 设置。`auto` 仅使用近期为免费 Zen/匿名模型验证的强度上限，不扩大探测范围；自动发现需开启主动探测，可能发送多次消耗额度的请求，结果两小时后过期。Go/付费模型及未验证强度不会被推算。

默认从真实流量学习可用性。只有 `models.active_probes: true` 才执行合成检查；管理端探测按钮仅安排检查，不绕过此开关。只有明确的模型下架/不存在证据才禁用模型，限流、认证失败、普通错误和超时不能证明下架。手动恢复会保持启用并退出自动探测，直到恢复自动控制。

### WARP

`warp.enabled` 默认 `false`。开启后，免费匿名/手动 Zen 流量及其目录/探测路径使用进程内 Cloudflare WARP 隧道；Go、付费及 OAuth 路由继续使用配置的代理。需要向 Cloudflare `2408` 端口发送 UDP，无需 TUN 设备或修改路由表。初始化失败会拒绝启动或候选配置。

`warp.pool_size` 是随并发调整的出口上限，范围 1–32，默认 2。`warp.stack` 为 `ipv6`（默认）、`ipv4` 或 `dual`。替换出口不保证获得新的上游额度。可选 `warp.source_ipv6` 要求 IPv6 堆栈及 `/64` 或更短前缀；Windows 下实际选择的源地址必须已分配到网卡，程序不会配置主机网络。

## 管理与 OAuth

WebUI 包含概览、诊断/Playground/日志、接入、API Key、模型与额度、轮转、配置、账号页面，提供简中、英语、越南语及意大利语。语言与主题选择保存在浏览器。完整操作与端点见[管理指南](docs/management.zh-CN.md)。

管理端使用管理员 Session Cookie、写操作 CSRF 和登录限流；修改管理员凭证会使 Session 失效。本地 API Key 不授予管理权限。`GET /api/config` 隐藏敏感值；再次验证密码后可显示手动 Key/代理值，但不会显示 OAuth Token。

在「模型与额度」启动 Device OAuth。自行打开页面显示的官方验证地址，输入 `user_code`，批准自己发起的请求，并按需选择组织/工作区。后端负责轮询和刷新；不使用粘贴 Token、导入本机凭证文件或提交回调 URL 的流程。

| 连接 | 范围与限制 |
| --- | --- |
| Go OAuth | 仅官方 Go 订阅路由；额度显示未知。 |
| Zen OAuth | 使用 Console 工作区 credits 的 PAYG；开始前必须明确确认，余额显示未知。 |

两类连接的待完成登录、凭证、刷新和断开分别管理。离开/隐藏页面停止自动轮询，返回可继续未过期登录。退出管理 Session 取消该 Session 的待完成登录，保留已完成连接；取消待完成登录不会断开已有账号。

连接以 `go_oauth`、`zen_oauth` 保存于配置，包含访问/刷新凭证，配置与备份均需保护。对应 OAuth 存在期间，手动 Key 保留但不作备用，即使需要重新登录也一样。断开仅删除本地连接，在重新发现后恢复手动路由，不撤销官方 Session。匿名关闭时，断开最后连接前先保留其他凭证。

只接受已认证官方远程配置中通过方案 URL 与 SDK 校验的路由，不从 Go 地址推导 PAYG，也不引入任意远程请求头。普通配置保存保留后端管理的 OAuth 凭证。旧 `go_subscription`/`zen_subscription` 加载时迁移；同一方案同时提供新旧字段会被拒绝。

实现依据管理指南列出的上游源码快照；此处说明代码行为，不代表所有账号的真实授权与推理已通过验证，上游权限和路由格式可能变化。

### 本地 Key、额度与轮转

API Key 页面支持创建、命名、启用、停用、删除。自动 Key 使用 192 位随机数；自定义 Key 为 16–256 位字母、数字、`.`、`_`、`-`，名称 1–80 字符，最多 200 个，至少一个启用。新请求立即使用变更；未实现按 Key 模型权限、消费上限或过期时间。

手动 Go Key 额度来自已配置 Go 上游的 `/v1/usage`，是共享滚动/每周/每月百分比窗口，不是逐模型余额。多个 Key 可能共享同一套餐，不能相加。成功缓存 60 秒、失败 15 秒，旧数据标记历史。OAuth 额度/credits 未知，不根据手动 Key 或成功请求推算。

轮转别名 `sota`、`sweet` 使用 Go 模型，支持三种消息接口和流式输出。每组共享当前模型，成功保持，失败推进。SOTA 固定包含 `deepseek-v4.1-flash`、`glm-5.3`、`grok-4.6`、`kimi-k3`、`qwen3.8-max`；Sweet 收集其他合适的 Go 模型。能否调用以实际发现为准。可在 `/#rotation` 调整别名、顺序和首输出超时。

普通错误预算为三个模型、每模型最多三次；额度/限流直接跳过，轮换不产生额外共享额度。流式内容提交后，错误结束当前答案并让下一请求使用后续模型。响应 `model` 保留请求别名，`X-Resolved-Model` 标记实际模型。状态保存于配置旁。

## 运行与安全

配置保存先校验并构建替代运行时，校验或持久化失败保留当前运行时。Key、代理、路由、模型与日志设置对新请求生效；监听地址和 WebUI 开关需要重启。手动编辑文件后执行 UI 重载或 `POST /api/config/reload`，没有自动文件监视器。

| 数据 | 存储位置 |
| --- | --- |
| 配置及上一版本 | `config.json`、`config.json.bak`。 |
| 模型及元数据缓存 | `config.json.models.catalog.json`、`config.json.models.dev.json`。 |
| 可用性及轮转 | `config.json.<upstream-fingerprint>.availability.json`、`config.json.rotation.json`。 |
| Session、用量、近期请求/尝试、日志环形缓冲、最近 Playground 结果 | 内存，进程重启清空。 |
| 结构化日志 | stdout JSON，需外部收集才能长期保存。 |

文件名以实际配置路径为前缀。监控保留跨热重载数据，不跨进程共享；Token 只统计上游报告值，不估算缺失用量。完整请求结果与收到响应头为止的尝试指标是不同口径。Playground 用量单独归类。最近一小时最多保留 10,000 请求/20,000 尝试，接口各返回最多 500 条。

`GET /healthz` 不发送上游请求、无需 Key。目录未就绪/为空、WARP 不可用或必要代理全部不健康时返回 503。可用但过期目录可返回 200 及 `status: "degraded"`；过期阈值为两倍刷新间隔，最低 60 秒。就绪不保证 Key 有效、额度充足或推理成功；公开响应只含计数与状态，不含凭证或代理地址。

本地使用将 API 与管理监听在 loopback。网络访问须使用 HTTPS 并限制管理入口；可信反向代理应正确设置 `X-Forwarded-Proto`。限流使用直连对端地址，同一反向代理后的客户端共享限制。可执行文件本身提供 HTTP。

配置和备份包含上游 Key、代理密码、OAuth Token，密码哈希不加密这些内容。限制文件权限，Windows 使用 ACL，分享源码时排除运行文件。`logging.dump_request_bodies` 默认关闭且需 debug 才输出，即使截断与脱敏仍可能暴露对话内容。Playground 和主动可用性探测会发送真实上游请求，可能消耗额度或 credits。

## 本地开发与核对

`cmd/opencode2api` 启动服务；`internal/admin` 负责管理，`gateway` 负责路由/运行时，`protocol` 负责 JSON/SSE 转换，`models` 负责发现，`oauth` 负责官方授权，`config` 负责校验/持久化，`telemetry` 负责监控/日志。静态资源位于 `webui`，无需安装前端依赖。

常规开发环境可运行以下原生 Go 命令；需要工作区内缓存时，将 `GOCACHE`、`GOMODCACHE` 设为项目内持久目录：

```bash
go vet ./...
go build -trimpath -o opencode2api ./cmd/opencode2api
```

Node.js 仅用于可选的 JavaScript 语法核对，无需安装包。在 Bash 执行，Windows 可用 Git Bash：

```bash
for file in webui/js/*.js webui/js/pages/*.js; do
  node --input-type=module --check < "$file" || exit 1
done
```

在 Bash 中执行以上命令（Windows 可用 Git Bash）。部署前检查见[验证与维护](docs/management.zh-CN.md#验证与维护)。发行版本可通过 `-ldflags "-X main.version=vX.Y.Z"` 注入。

| 现象 | 核对内容 |
| --- | --- |
| API 401 | 本地 Key、启用状态与请求头。 |
| 启动提示缺少凭证 | 配置上游 Key/已有 OAuth，或明确启用匿名。 |
| 无模型 / health 503 | 目录状态、原生协议支持、凭证配置和资源可用性。 |
| OAuth 请求失败 | 安全账号状态与重登提示，不会自动转 PAYG。 |
| HTTP 200 但生成失败 | SSE 错误事件、请求结果及 `x-request-id` 日志。 |
| 修改无效 | 保存/重载实际配置；监听变化需重启。 |
| Compose 中宿主配置修改无效 | 实际配置位于状态卷，重新导入或使用 WebUI。 |
| 重启后用量清零 | 计数器仅在内存，日志需另外保存。 |

升级前安全备份程序、配置及状态，停止进程后再替换可执行文件。回退时恢复相互匹配的程序、配置与状态。

## 致谢

感谢 [LINUX DO](https://linux.do) 社区的支持。

## 许可证

SPDX-License-Identifier: GPL-3.0-or-later。完整条款见 [LICENSE](LICENSE)，本软件不提供担保。
