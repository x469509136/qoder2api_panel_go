# 06 · 工具调用回读与守卫链

这是全项目最复杂、也最容易被低估的部分（约 3000 行）。问题来源：

> 模型在长会话里会**照着文本标记的格式**，把工具调用写成普通正文。
> 客户端把这段 JSON 当正文显示，本轮工具调用**不执行**。

网关的职责是：在**严格条件**下把这些"回声"还原成结构化 `tool_calls`；
在无法还原时**fail-closed**，绝不把内部标记透给用户，也绝不假装正常结束。

## 1. 文本化历史长什么样（`flatten_messages` 写入）

非结构化模式下，assistant 的 tool_calls 被序列化成：

```
<正文>

[assistant 请求调用工具]
[{"name": "Bash", "arguments": "{\"command\":\"ls\"}"}]
```

`LEAK_MARKER = "[assistant 请求调用工具]"`。

模型复述时会变形：

| 变体 | 例子 |
|---|---|
| 角括号 | `<assistant 请求调用工具>` |
| 混用 | `[<assistant 请求调用工具]` |
| 缺尾括号 | `[<assistant 请求调用工具` |
| 多余一层括号 | `[[assistant 请求调用工具]]` |

统一识别正则：

```python
_LEAK_MARKER_RE = re.compile(r"[\[<(]*\s*assistant\s*请求调用工具\s*[\]>)]?")
```

## 2. 三种需要还原的形态

### 2.1 marker + JSON 数组（本仓自己写入的格式）

```
[assistant 请求调用工具]
[{"name":"Bash","arguments":"{\"command\":\"ls\"}"},
 {"name":"Read","arguments":"{\"file_path\":\"/a\"}"}]
```

还原条件（**全部满足**）：

1. 标记完整，后面紧跟一个 JSON 数组
2. 数组元素都是 `{name, arguments}` 形态
3. `JSON.parse(arguments)` 成功且是**对象**
4. 每个 name 都在**本次请求声明的工具**里（`allowed_names`）

任一条不满足，按下面的"无法还原"处理。

### 2.2 DeepSeek 原生 DSML

```
<|DSML|function_calls>
  <|DSML|invoke name="Bash">
    <|DSML|parameter name="command">ls -la</|DSML|parameter>
  </|DSML|invoke>
</|DSML|function_calls>
```

正则（宽容到标签前缀可以任意）：

```python
TAG_START      = r"<[^>]*DSML[^>]*"
DSML_CALLS_RE  = TAG_START + r"calls>(.*?)</[^>]*DSML[^>]*calls>"
DSML_INVOKE_RE = TAG_START + r"invoke\s+name=[\"']([^\"']+)[\"']>(.*?)</[^>]*invoke>"
DSML_PARAM_RE  = TAG_START + r"parameter\s+name=[\"']([^\"']+)[\"'][^>]*>(.*?)</[^>]*parameter>"
```

**无 calls 包裹的形态**（issue #51：截图里出现过只有 invoke/parameter，或 calls
闭合标签被截断）也要能解析——直接在全文里找 invoke 块。

清洗时：

- 有 calls 包裹：把整个 match 替换成一个空格
- 无包裹：从第一个 invoke 之前的 `<` 起点裁起（保留前面的正文）

### 2.3 伪 XML 工具调用（issue #137）

```
<tool_call>Bash {"command":"ls"}</tool_call>
```

`parse_tool_call_tags(text, allowed_names)`。

## 3. 参数 JSON 的修复链

上游有几种真实的坏 JSON：

| 症状 | 修复 |
|---|---|
| 外层数组里内层 arguments 的引号没转义 | `_repair_json_quotes` / `_escape_unescaped_quotes` 补齐转义后重建 |
| 整体是字符串（多编码一层） | `_parse_tool_arguments` 再解一层（如果是对象/数组就采用） |
| 被截断 | 尝试 `_salvage_echo_array` 抢救；失败则判定不可还原 |
| 合法的非对象（数组/标量） | 包成 `{"value": <值>}`（Anthropic 侧要求 input 是对象） |

修复后**仍按同一套严格校验**重建；成功才还原成 `tool_calls`。

## 4. 无法还原时的判定（fail-closed）

| 症状 | 处理 |
|---|---|
| 被截断、无法还原的回声 | **按严格条件吞掉**，不把内部标记透给用户 |
| 数组结构合法，但内层参数非法 | **协议级失败**：流式发 error 帧，非流式 502，Responses 发 `response.failed`，Anthropic 发 `event: error` |
| 普通正文（包括**讨论**这些标记的正文） | 原样输出，不会吞掉正文 |
| 标记里的工具名全部**没在本次请求声明** | 判定为普通正文（`_echo_names_all_undeclared`） |

异常类型：

```python
class InvalidToolArguments(UpstreamStatus)      # 参数非法
class EmptyToolEcho(InvalidToolArguments)       # 只有标记没有内容
    # kind: "marker_only" | "unrecovered_dsml" | "truncated_arguments" | "falsified"
```

## 5. 流式三段判定（关键算法）

流式场景无法"看到全文再决定"，所以用**暂存 + 跨帧判定**：

```
猜测是标记的片段 → 先 buffer，不输出
  ├─ 确认不是标记 → 立即把 buffer 原样补发（一个字都不丢）
  ├─ 确认是完整标记 → 解析成 tool_calls 帧
  └─ 流结束仍未确认 → 按 fail-closed 收尾
```

### 5.1 前缀持有（hold）

`_leak_prefix_hold(text)`：文本是不是**可能成为** marker 的前缀。
例如 `[as` / `<assistant 请` / `[assistant 请求调用工` 都要 hold。

`_tool_echo_prefix_hold(text)`：同上，针对 `<tool_call` 和 DSML 标签前缀。

`_marker_starts_at(text)`：文本是否**已经**以 marker 开头。

### 5.2 同帧拆发（issue #16-B1）

如果同一帧里既有散文又有 marker：

```
_emit16, _hold16 = _find_marker_split(text_buffer)
if _hold16 and _emit16 and "<" not in _emit16:
    发出 _emit16                    # 散文不拖字
    text_buffer = _hold16           # marker 部分留下
```

含 `<` 的散文不走这条捷径（交给 DSML 剥离逻辑，`<` 可能是标签的开头）。

### 5.3 帧生成器链

```python
def recover_leaked_tool_calls(frames, allowed_names=None, audit=None):
    frames = _sse_normalize_tool_call_indices(frames)      # 1. index 归一
    leaked = _recover_leaked_tool_calls_inner(frames, allowed_names)  # 2. marker 回读
    dsml   = _sse_recover_dsml(leaked, allowed_names)      # 3. DSML 回读
    tagged = _sse_recover_tool_tags(dsml, allowed_names)   # 4. 伪 XML 回读
    with_ids = _sse_fill_tool_call_ids(tagged)             # 5. 补 id
    named  = _sse_guard_nameless_tool_calls(with_ids)      # 6. 无名调用守卫
    checked = _sse_guard_truncated_tool_args(named)        # 7. 截断参数守卫
    guarded = _sse_guard_unstructured_tool_echo(checked, allowed_names)  # 8. 残片守卫
    yield from _sse_guard_empty_tool_turn(guarded, allowed_names, audit) # 9. 空工具轮
```

**顺序即数据流方向**，每个阶段都是生成器，可单独测试。

### 5.4 tool_call.id 补全（issue #47）

上游偶尔发缺少 id 的增量帧。`_sse_fill_tool_call_ids`：

```
按 index 记住补出来的 id
新 index 且无 id → "call_" + uuid
同 index 后续帧 → 复用记住的 id
```

保证同一个调用分多帧时 id 不变（否则客户端会把它当成多个调用）。

### 5.5 index 归一（并行工具调用）

上游可能按 0,1,0,1 交替发（并行调用），也可能全部 index=0。
`_sse_normalize_tool_call_indices` 按"遇到新 id 就 +1"重排，
让客户端看到 0,1,2...

### 5.6 无名调用守卫（issue #124 / #116）

```
新 index 的首帧没有 name → 缓存参数，等 name 到达再发
流结束仍无 name → 整块丢弃
  ├─ 如果还有文本/思考内容 → finish_reason 降级为 stop
  └─ 如果什么都没有 → 按 fail-closed 收尾
```

## 6. 工具协议残片收尾策略（`QD_TOOL_ECHO_MODE`）

有些模型（实测 `Qwen3.8-Flash`）会把工具调用**当成正文**吐出来，随后流就结束了。

| 取值 | 行为 |
|---|---|
| `strict`（默认） | 以失败收尾（Responses 发 `response.failed`）。客户端应看到错误并重试 |
| `lenient` | **按正常完成收尾**并保留已产出的正文；日志打 WARN，`最近请求` 有一行 `lenient_tool_echo` |

lenient 是折中：把"整轮丢失"换成"这一轮没有工具调用、下一轮模型会重新发起"，
代价是客户端不知道残片被丢弃了。

面板也可切换：`POST /settings/tool-echo-mode`（持久化在 settings.json）。

## 7. Kimi 工具协议约束（issue #153）

Kimi（`kmodel` / `kmodel_latest`）的长工具链会反复展示双层 JSON 文本信封，诱导
模型照抄。处理：

1. **保留** `assistant.tool_calls` 与 `role:tool`（结构化直传）
2. assistant 工具消息的 `content` 保持**空字符串而非 null**
3. 存在工具声明且 `tool_choice != none` 时，在系统提示末尾追加固定的工具协议约束：
   - 原生调用通道
   - 参数 JSON 对象、正确转义
   - 遵守原 schema、完整闭合
   - 长写入按工具能力拆分

该段提示**不随步数变化**（否则会破坏前缀缓存），不覆盖客户端提示，不修改
schema、推理强度或采样参数。

## 8. 测试策略（可借鉴）

源仓库有 150 个套件，其中约 1/3 是这类守卫的回归：

| 套件 | 覆盖 |
|---|---|
| `_test_issue120_unrecovered_dsml.py` | DSML 残片 |
| `_test_issue120c_residue_edges.py` | 残片边界 |
| `_test_issue124_nameless_tool_call.py` | 无名调用 |
| `_test_issue137_tool_call_tags.py` | 伪 XML |
| `_test_issue153_kimi_tools.py` | Kimi 双层信封 |
| `_test_leak_guard.py` | 真实泄漏样本全链路 |
| `_test_stream_guards.py` | 守卫链 |
| `_test_stream_tool_ids.py` | id 补全（含"出口是包装器"的静态断言） |
| `_test_parallel_tool_indices.py` | 并行 index 归一 |

关键测试方法：**把真实出现过的泄漏样本送进完整流式管线**，断言：
1. 客户端看到的是结构化 tool_call，不是正文
2. 正文里**一个字都没丢**（散文部分逐字相等）
3. 无法还原时终态是错误帧，不是正常 `[DONE]`

## 9. Go 移植建议

这一块**建议 1:1 逐函数移植**，不要"用 Go 重写得更好"：

- 这些规则是**从真实泄漏样本反推出来的**，任何"简化"都会让某一类泄漏复发。
- 生成器链在 Go 里用 `func(yield func(T) bool)` 或 channel + goroutine 实现，
  建议用 **slice-of-frames 的纯函数**（下游是 `chan` 或 `iter.Seq`），
  这样每个阶段的单元测试只依赖输入输出，不依赖并发。
- 正则表达式直接照搬（Go 的 RE2 不支持回溯，但这些正则都是非回溯安全的）。
- `_recover_leaked_tool_calls_inner` 是唯一需要**跨帧状态**的阶段，建议用一个
  显式 `state` 结构体，而不是闭包变量（便于快照测试）。
