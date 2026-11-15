# 05 · 三协议双向转换

三种入口（Chat / Responses / Anthropic）最终都归一成 **Chat Completions 请求体**，
走同一条上游链路；返回时再按各自协议编码。

```
/v1/chat/completions ────────────────┐
/v1/responses     → responses_to_chat() ─┤→ open_upstream → 上游
/v1/messages      → messages_to_chat() ──┘
                                      │
上游 SSE ─┬→ chat 原样（清空字段）
          ├→ stream_responses_events()
          └→ stream_anthropic_events()
```

## 1. Chat Completions（主链路）

入站只做三件归一化：

| 函数 | 作用 |
|---|---|
| `normalize_tool_choice` | 上游只认 `auto`/`none`/`required`/具体函数 |
| `normalize_tools` | 出站统一嵌套 `tools[].function` 对象（Responses 的平铺形态上游会 400） |
| `translate_max_completion_tokens` | `max_completion_tokens` → `max_tokens` |

出站：

- **流式**：`sse_with_heartbeat(recover_leaked_tool_calls(iter_inner_sse(...)))`，
  最后补 `data: [DONE]`。
- **非流式**：`aggregate_stream()` → `chat.completion` 对象。

`stream_options.include_usage` 由客户端决定；上游的 usage 帧在流尾。

## 2. Responses API（Codex）

### 入站 `responses_to_chat(payload)`

| Responses 形态 | Chat 形态 |
|---|---|
| `instructions` | 首条 `system` 消息 |
| `input` 是字符串 | 一条 `user` |
| `input[]` 里 `type=message` | 按 `role` 转；`developer` → `system` |
| `type=reasoning` | 取 `summary[].text` 或 `content`，写给相邻 assistant 的 `reasoning_content` |
| `type=function_call` | `assistant.tool_calls[]`（与上一条 assistant 合并，不打断工具序列） |
| `type=function_call_output` | `{role:"tool", tool_call_id: call_id, content}` |
| `type=custom_tool_call` | `tool_calls[]`，参数包成 `{"input": "<raw>"}` |
| `type=custom_tool_call_output` | `role:"tool"` |
| 未知 type | **不静默丢弃**，打 WARNING（丢工具调用会让上游记录不一致） |

参数映射：`max_output_tokens` → `max_tokens`；`reasoning.effort`（或顶层
`reasoning_effort`）→ `reasoning_effort`；`parallel_tool_calls` 默认 `true`。

### 工具：custom freeform 的降级与还原

Codex 把文件编辑工具声明为 `{"type":"custom","name":"apply_patch","format":{...}}`，
上游 Chat 没有 custom 概念。处理：

**出站**：降级成单 `input` 字符串参数的 function，并在描述里追加固定提示：

```
This is a freeform tool. Put the COMPLETE raw payload into the single 'input'
string parameter, verbatim. Do not wrap it in JSON, do not wrap it in markdown
code fences, do not add commentary.
```

**入站**：名字在 `custom_names` 里的调用还原成 `custom_tool_call` 条目，
`input` 从 `{"input": "..."}` 里解包。

### namespace 工具（issue #127）

工具清单可能分两处（顶层 `tools` + `input[].additional_tools`），还可能包在
`{"type":"namespace","tools":[...]}` 容器里。

- 出站前 `_flatten_ns_tools()` 展平，并记下 `ns_map`
- `ns_map` 不下发上游，只用于回程给条目补 `namespace` 字段
- **必须在 `response.output_item.added` 帧就带上 namespace**，否则客户端按
  `(name, namespace, kind)` 匹配不到执行器

### 出站（非流式）`chat_to_response`

```
output = []
if reasoning:  output.append({id: rs_, type: "reasoning", summary: [...]})
for tc in tool_calls:
    custom ? {type:"custom_tool_call", input: <解包>}
           : {type:"function_call", arguments: <原字符串>}
if text or not output: output.append({type:"message", content:[{type:"output_text"}]})
```

**文本泄漏回退**：如果没有 `tool_calls` 但正文里有 DSML 或
`<tool_call>Name {json}</tool_call>`，解析成 `function_call` 条目并清洗正文。

### 出站（流式）`stream_responses_events`

事件序列（带严格单调 `sequence_number`）：

```
response.created
response.in_progress
  [reasoning]  response.output_item.added (reasoning)
               response.reasoning_summary_part.added
               response.reasoning_summary_text.delta × N
               response.reasoning_summary_text.done
               response.reasoning_summary_part.done
               response.output_item.done
  [tools]      response.output_item.added (function_call / custom_tool_call)
               response.function_call_arguments.delta × N
               或 response.custom_tool_call_input.delta × N
  [text]       response.output_item.added (message)
               response.content_part.added
               response.output_text.delta × N
               response.output_text.done
               response.content_part.done
               response.output_item.done
response.completed / response.incomplete / response.failed
```

`seq` 从 `holder["seq"]` 恢复 —— 上游重开导致生成器重建时，客户端看到的
`sequence_number` 不能回退。

`response.failed` 用于工具参数非法等协议级失败；`_responses_completed_quiet`
用于"没有 content 但要正常收尾"的场景。

## 3. Anthropic Messages（Claude Code）

`qoder_anthropic.py` 的设计边界（写在文件头）：

1. **只做协议翻译，不做形态决策**：输出永远是标准 chat 形状；
   "结构化直传 vs 文本信封"由调用方单点决定（`_qd_structured`）。
2. **吞掉/回读守卫不在此重写**：输入契约是"已被 `recover_leaked_tool_calls`
   清洗过的 chat chunk"。
3. **不生成** Anthropic 的 thinking signature。

### 入站 `messages_to_chat`

| Anthropic | Chat |
|---|---|
| `system`（字符串 / 块数组） | 首条 `system` |
| `messages[].role == "system"` | 按 `QD_ANTHROPIC_MSG_SYSTEM` 处理 |
| `content` 是字符串 | 单 text 块 |
| `content` 是 dict | 当单元素列表（旧写法会静默吞掉图像块） |
| `type=text` | 拼进正文 |
| `type=image`（base64 / url） | `image_url` part（base64 转 data URI） |
| `type=tool_use` | `tool_calls[]`，`input` 序列化成 JSON 字符串 |
| `type=tool_result` | `{role:"tool", tool_call_id: tool_use_id, content}` |
| `type=thinking` | **一律丢弃** |

**顺序**：`tool_result` 先落（紧跟其 tool_use 的 assistant）→ assistant →
再 user。只承载 tool_result 的消息**不追加空消息**（否则历史里留 `content:""`
的幽灵轮次）。

`max_tokens` 必填（与 chat 相反）；缺失返回 400。

### system 三形态（issue #55）

三条来源必须全部合并且放最前：

1. 顶层 `system`（字符串或块数组）
2. `messages` 内的 `system` 消息

`QD_ANTHROPIC_MSG_SYSTEM` 三档：

| 值 | 行为 | 缓存影响 |
|---|---|---|
| `inline`（默认） | messages 内 system **就地**降级成 user 消息，位置不变 | ✅ 前缀稳定 |
| `merge` | 合并进最前面的 system（旧行为） | ❌ 动态内容污染前缀，实测 92% → 14.7% |
| `drop` | 丢弃 | 丢失信息 |

### 思考档位映射（`effort_from_anthropic`）

```
thinking.type == "disabled"           → None（不下发）
thinking.effort                       → 直接取（如果在档位表里）
output_config.effort                  → 直接取
thinking.budget_tokens                → 断点映射
```

`budget_tokens` 断点（issue #62）：

| budget | 档位 |
|---|---|
| ≤ 1024 | minimal |
| ≤ 4096 | low |
| ≤ 16384 | medium |
| ≤ 32768 | high |
| > 32768 | max |

Claude Code 对**第三方模型名**发的是 `thinking={"type":"adaptive"}` +
`output_config={"effort":"low|medium|high|max"}` —— adaptive 是合法 dict 但没有
effort/budget，旧实现因此跳过 output_config 分支，四档全部映射成 None
（静默固定跑默认档）。

**clamp 必须双向就近**（issue #126）：同距时偏向模型默认档。
旧实现只向下取，`dfmodel`（low/high/max）上的 medium/high 会被压成 low —— 静默降档。

### 出站（非流式）`chat_to_messages`

```
{id: msg_..., type:"message", role:"assistant", model,
 content: [text?] [thinking?] [tool_use...],
 stop_reason, stop_sequence: null, usage}
```

- `thinking` 块带**空 signature**，且只在 `QD_ANTHROPIC_THINKING=empty` 时输出
- **空 name 的 `tool_use` 直接丢弃**（issue #116）：空 name 会让下游整轮
  malformed —— 宁可不还原这一个调用，也不发坏块

### 出站（流式）`stream_anthropic_events`

```
message_start
  content_block_start (thinking) → content_block_delta(thinking_delta) × N
  content_block_start (text)     → content_block_delta(text_delta) × N
  content_block_start (tool_use) → content_block_delta(input_json_delta) × N
  content_block_stop × N
message_delta   ← usage 是**累计完整用量**（input/cache_read/cache_creation/output）
message_stop
```

两个实现细节：

1. **`input_json_delta` 是增量字符串**，逐片拼接、不要每片 `json.loads`。
2. **工具块要缓冲到拿到 name 才发 `content_block_start`**（issue #116）：
   Anthropic 协议不允许改已发出块的 name。期间到达的 arguments 先攒着，
   start 之后按序补发。流结束仍无 name 的块整块丢弃，并把
   `stop_reason` 从 `tool_calls` 降级为 `stop`（避免下游一直等一个永远不会到来的
   工具结果）。

`message_delta.usage` 必须是**累计完整用量**（issue #42）：Claude Code / AI SDK
按它读最终计量，只回 `output_tokens` 会让客户端输入 token 恒 0。

### usage 换算口径（`chat_usage_to_anthropic`）

```
cached     = cached_tokens | prompt_cache_hit_tokens | completion_tokens_details.cached_tokens
             | prompt_tokens_details.cached_tokens
cached     = clamp(cached, 0, prompt_tokens)

input_tokens                = prompt_tokens - cached
cache_read_input_tokens     = cached
cache_creation_input_tokens = 0          ← 上游没这个概念，不编造
output_tokens               = completion_tokens
```

守恒判据：`input_tokens + cache_read_input_tokens == prompt_tokens`。

### count_tokens（本地估算，不联网）

```
cjk   = 文本里 ord(ch) > 0x2E80 的字符数
other = 总字符数 - cjk
input_tokens = max(1, cjk + (other + 3) // 4)
```

## 4. 三个入口共用的前置检查

`_model_precheck(model, realm)` —— 顺序固定，**三处入口共用**（新增入口必须挂上，
否则会出现"chat 被拦、别的协议绕过"）：

1. 跨区错配：`模型只在 A 区提供，但 Key 绑定的是 B 出口` → 400
2. 全局封禁：`banned_models` fnmatch → 400
3. Key 白名单：`api_keys[].models` fnmatch → 400

## 5. 请求头支持

| 头 | 作用 |
|---|---|
| `X-Realm: cn\|intl` | 指定出口 |
| `?realm=` | 同上 |
| `X-Conversation-Id` / `?conversation_id=` / body 里的会话字段 | 会话亲和键 |
| `X-QD-Context: 1M\|400K\|200K\|<tokens>` | 抬高上下文窗口（issue #106） |

`X-QD-Context` 会**先无条件清掉 body 里的 `_qd_context` 键**：客户端不能靠伪造
body 字段提窗，只有 HTTP 头能设置。
