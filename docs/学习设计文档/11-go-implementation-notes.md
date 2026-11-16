# 11 · Go 实现要点与差异清单

本篇是给维护者的速查：Go 版里**每个关键算法落在哪个文件**，以及相对 Python 版
的明确差异。

## 1. 一次请求的代码路径

下表中的包路径均相对仓库的 `code/` 目录。

```
POST /v1/chat/completions
  server.routeRequest                       server/server.go
    └ requireKey                            server/server.go
    └ handleChat                            server/chat.go
       ├ NormalizeToolChoice/Tools/MaxTokens  protocol/types.go
       ├ modelPrecheck                      server/chat.go
       ├ extractSessionKey / promptFingerprint
       ├ openUpstreamFor                    server/upstream_path.go
       │  ├ route.Resolve                   route/route.go
       │  ├ detectModelRealm                server/chat.go
       │  ├ catalog.ExclusiveRealm           catalog/catalog.go
       │  ├ pool.PickForSession              accounts/pool.go
       │  ├ buildBody                       server/chat.go
       │  │  └ protocol.BuildQoderBody       protocol/build.go
       │  │     └ FlattenMessages             protocol/flatten.go
       │  │     └ PrepareInlineImages         protocol/images.go
       │  ├ sessionCache.Resolve            session/session.go
       │  └ upstream.Post                   upstream/upstream.go
       │     └ sign.QoderEncode / Session    sign/sign.go
       │     └ sign.Session.Headers           sign/sign.go
       ├ aggregateStream / streamChat        server/chat.go
       │  └ upstream.Stream                  upstream/upstream.go
       │  └ guards.Process                   guards/frames.go
       └ recordUsage                         usage/usage.go
```

## 2. 算法落点

| 算法 | 文件 | 关键函数 |
|---|---|---|
| 自定义 Base64 | `sign/sign.go` | `QoderEncode` / `QoderDecode` |
| AES-128-CBC（key==iv） | `sign/sign.go` | `AESCBCEncrypt` / `AESCBCDecrypt` |
| RSA PKCS#1 v1.5（1024 位） | `sign/sign.go` | `RSAPKCS1v15Encrypt`（`math/big` 模幂） |
| AES-GCM（桌面凭证） | `sign/sign.go` | `AESGCMDecrypt` / `ChromiumDecryptV10` |
| COSY 会话与 Bearer | `sign/sign.go` | `NewSession` / `Session.Bearer` / `Session.Headers` |
| 键排序紧凑 JSON | `sign/sign.go` | `JSONSortedCompact` |
| 设备身份派生 | `fingerprint/fingerprint.go` | `DeriveID` 族 |
| 会话压平 | `protocol/flatten.go` | `FlattenMessages` |
| 工具结果截短 | `protocol/flatten.go` | `shrinkToolResult` |
| 结构化 vs 文本化决策 | `protocol/flatten.go` | `StructuredToolHistoryEnabled` |
| baseprompt 覆写 | `protocol/build.go` | `BuildQoderBody` |
| 图片归一（PNG IDAT 重压） | `protocol/images.go` | `normalizeDataURI` / `recompressPNG` |
| 信封解包 | `upstream/upstream.go` | `Stream` |
| 队列等待解析 | `upstream/errors.go` | `QueueWaitSeconds` |
| 错误分类 | `upstream/errors.go` | `IsTransientUpstream` / `IsRequestFault` |
| marker 回读 | `guards/leak.go` | `ParseLeakedToolCalls` |
| DSML 回读 | `protocol/leakparse.go` | `ParseDSMLToolCalls` |
| 伪 XML 回读 | `guards/tags.go` | `ParseToolCallTags` |
| 参数 JSON 修复 | `guards/jsonfix.go` | `RepairArgsJSON` / `EscapeUnescapedQuotes` |
| 守卫链 | `guards/frames.go` | `Process` |
| Anthropic 入站 | `protocol/anthropic.go` | `MessagesToChat` |
| Anthropic 出站（流） | `protocol/anthropic_stream.go` | `AnthropicStreamState` |
| Responses 入站 | `protocol/responses.go` | `ResponsesToChat` |
| Responses 出站（流） | `protocol/responses_stream.go` | `ResponsesStreamState` |
| 双区选区 | `server/chat.go` | `detectModelRealm` |
| 失败治理三族 | `accounts/account.go` | `NoteSoftRate` / `NoteFailure` / `NoteUnknownFailure` |
| 护栏 | `accounts/account.go` | `reserveBlocked` / `dailyLimitBlocked` / … |
| 额度归一 | `accounts/quota.go` | `NormalizeQuotaPayload` |
| 前缀会话亲和 | `session/session.go` | `MemoryCache.Resolve` / `RedisCache.Resolve` |
| 调度到点即补 | `scheduler/scheduler.go` | `nextTarget` / `occurrences` |
| 用量报表 | `usage/usage.go` | `StatsReport` / `PerfStats` / `DailyReport` |
| SSRF 守卫 | `webtools/webtools.go` | `CheckURL` / `openPinned` |

## 3. 与 Python 版的差异清单

### 3.1 行为等价但实现不同

| 项 | 说明 |
|---|---|
| 密码学 | 换标准库；RSA 仍手写模幂以支持 1024 位 |
| 图片处理 | 不再需要 Pillow；只重压 PNG 的 IDAT，且必须小 20% 才替换 |
| 并发 | 每请求一 goroutine |
| 配置写回 | `settings.Store.Update` 保证"读-改-写"在锁内，且改写前校验可解析 |

### 3.2 明确不同（有意）

| 项 | Python | Go | 影响 |
|---|---|---|---|
| 自更新 | `/update/apply` 可用 | 只报告状态，`can_apply=false` | 运维改用流水线 |
| UMID 原生身份 | 调 `runtime-info` 取真实机器身份 | 只发派生身份 | 签到链路不发机器头（安全侧） |
| `/agents/cli` | 返回可执行配置器源码 | 返回说明页 | 远程机器改用 `/agents/template` |
| `X-Qoder-Realm` 响应头 | 每次请求实测出口 | 全局出口 | 客户端仍可见区域，但流内自动换区不单独标注 |
| 流内信封重开 | chat 链路支持 2 次 | 不支持（返回错误帧） | Anthropic 原本也不支持；chat 端退回重试由客户端完成 |

### 3.3 尚未实现（后续可补）

| 项 | 位置 | 备注 |
|---|---|---|
| 严格"流内信封重开" | `server/chat.go` | Python 在已建流但未发字节时重开 |
| 团队流量守卫（并发闸门 + 最小间隔） | `accounts/account.go` 有字段 | 未接入选号路径 |
| 前缀保温 | `keepalive/` 完整实现 | 默认关闭，与 Python 一致 |
| 活动领取的 `only_kinds` 过滤 | `tasks/` | 当前领全部可领项 |
| 面板完整 i18n 切换 | `server/panel.go` | 只暴露 en 一包 |

这些都不影响主链路可用性，且都已在代码里留了结构位置。

## 4. 已知的坑（踩过并修掉）

| 坑 | 现象 | 修法 |
|---|---|---|
| RE2 无前瞻 | 启动即 panic | 三处 `(?=...)` 改为捕获分隔符 |
| Mutex 不可重入 | 保存路由配置时卡死 | `Save` 不跨调用持锁 |
| Windows 时钟精度 | 同一 tick 两个会话撞同一个上游 session | 会话 id 改用 `crypto/rand` |
| LRU 只淘汰过期项 | 缓存无界增长 | 超上界直接淘汰最旧项 |
| 本地错误被归类成上游瞬时故障 | 用户去查网络而不是查账号 | `friendlyUpstreamError` 先认本地前缀 |
| `row` 按值传递 | 算出的 tokens/s 读不到 | 测试改为从日志回读 |

## 5. 手工验证清单

启动后按顺序发这些请求，可覆盖大部分链路：

```bash
# 1. 探活
curl -s localhost:18090/ping

# 2. 面板状态（默认口令）
curl -s localhost:18090/panel/status

# 3. 模型清单（应含官方 display_name 作为 id）
curl -s localhost:18090/v1/models | head -c 400

# 4. 无账号时的报错必须指向账号页，而不是"上游故障"
curl -s -X POST localhost:18090/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"Qwen3.8-Max","messages":[{"role":"user","content":"hi"}]}'

# 5. Anthropic 必填校验
curl -s -X POST localhost:18090/v1/messages \
  -H 'Content-Type: application/json' \
  -d '{"model":"Qwen3.8-Max","messages":[{"role":"user","content":"hi"}]}'

# 6. 本地 token 估算（不发上游）
curl -s -X POST localhost:18090/v1/messages/count_tokens \
  -H 'Content-Type: application/json' \
  -d '{"model":"x","max_tokens":8,"messages":[{"role":"user","content":"你好"}]}'
```

接上真实账号后，再验证：

```bash
# 非流式
curl -s -X POST localhost:18090/v1/chat/completions \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"model":"Qwen3.8-Flash","messages":[{"role":"user","content":"你好"}]}'

# 流式（应看到 data: 帧，最后 data: [DONE]）
curl -N -X POST localhost:18090/v1/chat/completions \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"model":"Qwen3.8-Flash","stream":true,"messages":[{"role":"user","content":"你好"}]}'

# Anthropic 流式
curl -N -X POST localhost:18090/v1/messages \
  -H "x-api-key: $KEY" -H 'Content-Type: application/json' \
  -d '{"model":"Qwen3.8-Flash","max_tokens":64,"stream":true,
       "messages":[{"role":"user","content":"你好"}]}'
```

## 6. 排障入口

| 现象 | 先看 |
|---|---|
| 401 且面板正常 | `/health.env.clock_skew_s`（时钟漂移会让 COSY 签名失效） |
| 429 / 503 排队 | `/health.risk`，`POST /diag/queue` |
| 缓存命中率低 | `/usage/stats` 的 `cache` 与 `full_price` |
| 工具调用中断 | `/usage/perf.tool_guards`（`recovered` / `dropped`） |
| 额度显示 0 | `/accounts/credits` 看是否命中候选端点；`/credits/probe` |
| 签到无效 | `POST /diag/checkin` |
