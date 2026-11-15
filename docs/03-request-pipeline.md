# 03 · 请求构造与上下游链路

这一篇讲"客户端请求 → 上游请求体 → 上游 SSE → 客户端响应"的完整字节旅程。

## 1. 上游端点

```
CHAT_PATH   = /algo/api/v2/service/pro/sse/agent_chat_generation
              ?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1
MODELS_PATH = /algo/api/v2/model/list?Encode=1
```

主机按区域选（`gateway_candidates`）：

| 区域 | 主 | 备 |
|---|---|---|
| CN | `gateway.qoder.com.cn` | 无（企业 VPC 时只有实例专属域） |
| INTL | `api3.qoder.sh` | `api2.qoder.sh` → `api1.qoder.sh` |

因为签名只覆盖 path，换主机无需重签。

## 2. baseprompt.json：官方请求体骨架

`build_qoder_body()` 每次 `deepcopy` 这份模板再覆写。模板结构：

```
{
  request_id, request_set_id, chat_record_id, session_id,
  stream: true,
  chat_task: "FREE_INPUT",
  is_reply: true, is_retry: false,
  code_language: "", source: 1, version: 3, chat_prompt: "",
  session_type: "qodercli", agent_id: "agent_common", task_id: "common",
  aliyun_user_type: "personal_standard",
  model_config: {key, display_name, model:"", format:"openai", is_vl,
                 is_reasoning, api_key:"", url:"", source:"system",
                 max_input_tokens},
  chat_context: {
    chatPrompt, features: [],
    text: {type:"text", text},
    extra: {context: [], originalContent: {type:"text", text},
            modelConfig: {...}},
    imageUrls: null
  },
  image_urls: null,
  parameters: {max_tokens: 32768},
  messages: [ {role:"system", content:"You are Qoder, an interactive CLI tool…"} ],
  tools: [Bash, ... 14 个官方 agent 工具],
  business: {product:"cli", version, type:"agent", id, name, begin_at, stage:"start"}
}
```

### 覆写规则（按代码顺序）

1. **request/session id**：全部新 UUID。
   - `request_id`：每请求唯一（计费用）
   - `chat_record_id` / `business.id`：**同一段对话复用**（issue #128）。
     官方用量面板按它归组，每请求新 UUID 会把一个 agent 回合拆成几十条
     "<1min 的对话"，日上限（code 110）被迅速刷爆。
   - `session_id`：由前缀会话缓存决定（见 07 篇），兜底用会话级 id。
2. **messages**：
   - 模板自带的示例 user 轮次**必须丢弃**，只保留它的 system 基座。
   - 客户端没给 system 时：优先 `--system-prompt` 覆盖值（非默认时），否则保留
     模板自带的 Qoder 系统提示词。
   - 然后追加压平后的真实会话。
3. **model_config**：
   - `key` = 解析后的上游 key（如 `qmodel_38max`）
   - `display_name` = 官方此刻的名字（动态清单优先）
   - `is_vl` / `is_reasoning` / `max_input_tokens` 来自官方清单
   - `max_input_tokens` 可被 `X-QD-Context` 头或面板设置抬高（1M/400K/200K）
4. **chat_context**：`text.text` 与 `extra.originalContent.text` 都填**最新一条
   user 消息**（上游用它做高亮和会话名）；`extra.modelConfig` 是 `model_config` 副本。
5. **parameters**：`max_tokens`（先按已学到的上限 clamp）、`reasoning_effort`
   （归一化到模型支持的档位）。
6. **tools**：客户端给了就用客户端的（custom freeform 已降级），否则置空——
   **不能**把 Qoder 桌面的 14 个 agent 工具泄漏给普通客户端。
7. **business**：
   - `id` / `begin_at`：会话级复用
   - `name` = 最新 user 消息前 30 字
   - `model_key` = 上游 key（**计费归因读它**，不发会被静默记成 auto）
   - `version` = `COSY_VERSION`（身份故事应与 cosy-version 同源）
8. **aliyun_user_type**：取账号真实值，缺省 `personal_professional_trial`。

## 3. 会话压平（`flatten_messages`）

签名：`flatten_messages(messages, keep_reasoning=False, structured=False)`
→ `(system_text, flat_msgs, images)`

### 规则表

| 输入 | 输出 |
|---|---|
| 首个 `system`/`developer` | 抽为 `system_text`（其余 system 丢弃） |
| `user` / `assistant` 文本 | `{role, content: 字符串}` |
| `tool` 结果，`structured=True` | `{role:"tool", tool_call_id, content}`，content **恒为空串而非 null** |
| `tool` 结果，`structured=False` | 降级成 `{role:"user", content: "[工具结果(name)]\n..."}` |
| assistant 带 `tool_calls`，`structured=True` | 原样透传 id/type/function，arguments 统一成 JSON 字符串 |
| assistant 带 `tool_calls`，`structured=False` | 序列化成 `content + "\n\n[assistant 请求调用工具]\n" + JSON 数组` |
| `reasoning_content` | 仅 `keep_reasoning=True`（DeepSeek 族）时保留 |
| 带图片的消息 | 保留 OpenAI vision parts 形态 |
| 未知角色 | 降级为 `user`，不静默丢弃 |

三个非直觉的细节：

1. **`content` 用空字符串不用 `null`**（task-31）：`null` 会让上游转换层丢掉配对的
   assistant 消息，DeepSeek/Kimi 直接 `provider_error`。
2. **图片只能放消息 content 的 parts 里**：实测顶层 `image_urls` /
   `chat_context.imageUrls` 上游**不读**——填了模型仍回"我没有收到图片"。
   而且顶层冗余会让 10 张图 15MB → 出站 48MB，撞阿里云 SLB 30MB 限制被 RST。
   `QD_KEEP_TOPLEVEL_IMAGES=1` 可恢复旧行为（仅排查用）。
3. **历史工具结果截短**（见下）直接影响缓存命中率。

### 历史工具结果截短

```
QD_TOOL_RESULT_KEEP   = 2000  头尾各保留字符数（0/off = 不截）
QD_TOOL_RESULT_SCOPE  = tail  截短范围
QD_TOOL_RESULT_KEEP_TAIL = 1  tail 模式下保留完整的最后几条
```

三种 scope：

| scope | 行为 | 缓存影响 |
|---|---|---|
| `tail`（默认） | 只保护最后 N 条工具结果，更早一律截 | **字节恒定**：一条结果一旦被截，之后每轮生成完全相同 |
| `all` | 全部截短 | 恒定 |
| `turn`（旧） | 按"当前轮起点"滑动 | ❌ 用户每发一条消息就把全部历史重削一遍 → 同一条消息在不同轮次序列化不同 → 上游前缀缓存整段失效（实测 95% → 8%） |

截短后的省略标记是**固定字符串，不含字数**，这是字节稳定的关键。

### 结构化 vs 文本化决策

`structured_tool_history_enabled(model, model_key, realm, messages)`：

- `QD_STRUCTURED_TOOL_HISTORY=auto`（默认）：CN/INTL 的 Qwen / GLM / DeepSeek / Kimi
  白名单用结构化
- `on` / `off` 强制
- `QD_STRUCTURED_TOOL_HISTORY_REALMS` 可缩小区域范围
- **只要有一个 `tool_call_id` 不完整，整个请求退回文本化**（`_tool_ids_ok`）

设计原则是**单点决策**：Anthropic 入口会把算好的结果放在 `payload["_qd_structured"]`
下传，`build_qoder_body` 优先采信它。两处各判一次会导致"桥接层以为结构化、
flatten 却走文本化"，症状正是客户端收到信封文本。

## 4. 图片预处理（`qoder_images`）

| 项 | 值 |
|---|---|
| 开关 | `QD_IMAGE_TRANSCODE=auto`（默认开） |
| 触发 | ≥ 256 KiB 的 PNG/BMP/TIFF |
| 不缩分辨率、不移除任何图片 | ✅ |
| 非透明图 | 可转 quality=95、4:4:4 JPEG，且必须比原图小 20% 才替换 |
| 透明图 | 保留无损 PNG |
| 原样保留 | 小图、JPEG、动画、带 EXIF/ICC/色彩/DPI 元数据、16 位图、解码失败 |
| 远程 URL | 不下载、不改写 |
| 缓存 | 内存 LRU 32 条 / 32 MiB，不落盘 |

目的只有一个：**同一张图跨轮与重试产生完全相同的字节**（缓存前缀稳定），
同时减少"图片转换超时"。

## 5. 上游响应的信封格式

上游返回的 SSE 不是标准 OpenAI chunk，而是**嵌套信封**：

```
data: {"headers":{...},"body":"<内层 OpenAI chunk JSON 字符串>","statusCodeValue":200}
data: {"body":"[DONE]"}
event:finish{...}          ← 计时元数据，忽略
```

`iter_inner_sse(resp, holder)` 的处理：

1. 剥掉所有 `data:` 前缀（可重复出现）
2. 解析外层 JSON
3. `statusCodeValue != 200` → 抛 `UpstreamStatus(status, body)`
4. `body == "[DONE]"` → 结束
5. 解析内层 JSON，记 usage 到 `holder["usage"]`（只取第一次）
6. 调 `note_inner_upstream_error` 只做观测
7. `clean_chunk` 清洗后输出 `data: {...}\n\n`

### clean_chunk 清洗什么

| 情况 | 处理 |
|---|---|
| `delta.function_call` 存在但 name 为空 | 删除 |
| `delta.tool_calls` 是空数组 | 删除 |
| `delta` 里的 `extra_fields`/`refusal`/`reasoning_content` 为假值 | 删除 |
| delta 洗空了且没有 `finish_reason` | 整帧丢弃 |
| usage 缺标准位 `prompt_tokens_details.cached_tokens` | 从 `cached_tokens`/`prompt_cache_hit_tokens`/`completion_tokens_details.cached_tokens` 搬过来（**原字段保留**） |

最后一条很重要：上游按模型族换字段（DeepSeek 用 `prompt_cache_hit_tokens`，
Qwen 用 `completion_tokens_details.cached_tokens`），按标准协议解析的客户端恒读 0。

### 内层错误（HTTP 200 里的错误）

形态：HTTP 200 + 信封 `statusCodeValue=200`，但**内层 chunk 带 `error`**，例如
`provider_error → invalid_request_error`。既有路径对此完全无感，客户端只看到
200 的空正文，排障成本极高。

处理：只计数 + 按类别首次 WARN，**绝不改行为**。分类：

| kind | 关键词 |
|---|---|
| `content_policy` | datainspectionfailed / inappropriate content / contentfilter |
| `rate_limit` | rate_limit / throttl / frequency limit / 10605 / too many |
| `invalid_request` | invalid_request / invalid_parameter / provider_error / must be a response |
| `auth` | unauthor / forbidden / permission / expired / token |
| `other` | 其余 |

计数暴露在 `/usage/perf` 的 `inner_errors`。

## 6. 流式传输

### 6.1 chunked + keep-alive

```python
self.send_header("Transfer-Encoding", "chunked")
...
self.wfile.write(("%x\r\n" % len(data)).encode()); self.wfile.write(data); self.wfile.write(b"\r\n")
...
self.wfile.write(b"0\r\n\r\n")   # 结束
```

不用 `Connection: close` 的原因见 01 篇。`_sse_begin` 是**幂等**的：排队可视化会
提前开流，后续调用不能重复发头。

### 6.2 心跳

上游首字延迟可达 40–70 秒（高思考档位 + 长上下文）。这期间网关每 5 秒发一个
SSE 注释帧 `: ping`（`QD_SSE_HEARTBEAT`，0 = 关闭）。

实现是一个**读线程 + 队列**：

```python
box = queue.Queue(); done = object()
def _reader():
    try:
        for item in source: box.put(item)
    except BaseException as exc: box.put(exc)
    finally: box.put(done)
threading.Thread(target=_reader, daemon=True).start()

while True:
    try: item = box.get(timeout=interval)
    except queue.Empty:
        send(b": ping\n\n"); waited += interval
        if waited >= idle_limit: raise TimeoutError(...)
        continue
    if item is done: return
    if isinstance(item, BaseException): raise item
    waited = 0.0
    yield item
```

关键点：**顺序保证**（心跳只在队列空闲时插入）、**错误传播**（上游异常在主线程
重新抛出，不是吞掉）、**空闲上限**（默认 900 秒）。

### 6.3 流内信封重试

HTTP 200 已建立流，但 SSE 信封里是 418/5xx —— 只要**还没向客户端写出任何字节**
（`emitted == False`）就重新请求上游，最多 2 次（`should_retry_envelope`）。

Anthropic 入口**故意不做**这件事：Anthropic 客户端对重复 `message_start` 的
容忍度未知，宁可给明确的 `event: error` 收尾。

## 7. 非流式聚合（`aggregate_stream`）

把信封流折叠成一个 `chat.completion`：

- 累加 `content` / `reasoning_content`
- 按 `index` 合并 `tool_calls` 的增量 arguments
- 记录 `finish_reason`、`usage`、首帧时间（算 TTFT）
- 输出前跑同一套泄漏回读守卫

`aggregate_with_envelope_retry` 在外层包一层：此时尚未向客户端写任何字节，
信封重试绝对安全。

## 8. 出站体积两道闸

| 闸 | 位置 | 阈值 | 行为 |
|---|---|---|---|
| 客户端请求体 | `_read_payload` | `QD_MAX_PAYLOAD_BYTES`（50 MB） | 413 |
| 发往上游的编码体 | `open_upstream` | `QD_MAX_BODY_MB`（29 MiB） | **本地 413，不发请求、不换号、不冷却账号** |

第二道闸存在的理由（issue #59）：阿里云 SLB 对单个 POST body 有 ~30MB 硬限制，
超了直接 RST。本地表现为 Broken pipe，会被误判成瞬态故障反复重试，最终把整个
账号池打到熔断。

实现技巧：编码前用 `4*((n+2)//3)` 估算编码后大小，不真的分配几十 MB 字符串。

## 9. 模型清单四级降级（`fetch_models`）

```
1. 官方动态接口 /algo/api/v2/model/list   （需要账号，COSY 签名）
2. 磁盘缓存 accounts/models-cache-<realm>.json  （7 天有效）
3. 本机官方客户端缓存 ~/.qoder*/.models/<uid>/catalog-v7（v6 兜底）
4. 仓库内置快照 qoder_catalog_cn.json / qoder_catalog_intl.json
```

- 内存缓存 5 分钟；`GET /models/refresh` 立即刷新。
- 拉取动态清单时**必须带 body**（`qoder_encode("{}")`），裸 GET 会 403 ——
  因为签名覆盖了 body。
- 动态清单一到手就注册"显示名 → key"，解决官方改名窗口期
  （如 `kmodel_latest` 从 Kimi-K3 改名 Kimi-K3.1-Pro）。
- 对话目录取 `chat` 并补充 `app` 独有条目；同 key 以 `chat` 的元数据为准。

`/v1/models` 的每条是"信息量很大"的对象：`upstream_key`、`aliases`、`name_local`、
`description`、`capabilities`（vision/tool_calls/reasoning，四种别名同义重复）、
`context_windows`/`context_window_default`、`reasoning_efforts`/
`reasoning_default_effort`、`price_factor`/`price_factor_peak`/`price_factor_valley`/
`off_peak`/`off_peak_active_now`、`enabled`/`disabled_reason`、`realm`/`realm_exclusive`。

**绝不编造字段**：官方清单没有 `maxOutputTokens`，只有当"学到的上限"
（见 08 篇）存在时才输出。

## 10. 峰谷价推导

```
peak   = promotion.before_promotion_price_factor  或  original_price_factor
valley = round(peak * promotion.discount_factor, 4)   （discount ∈ (0,1]）
valley = valley or 当前 price_factor
off_peak_active_now = 按 promotion.timezone + window_start/end 本地复算
```

注意：**低谷价必须由峰值 × 折扣推导**，不能直接用"当前 price_factor"——
快照若在非低谷时段抓取，两者相等，看板/客户端会算错费用。
