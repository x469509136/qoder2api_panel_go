# 08 · 用量、积分与可观测性

## 1. 存储布局

```
usage/
  usage.jsonl            每请求一行（append-only）
  usage-summary.json     累计汇总（原子重写）
  max-output-limits.json 学到的输出上限（7 天 TTL）
  pricing-overrides.json 用户手填价格
  activity.jsonl         签到/领取动作流水（5MB 轮转，30 天保留）
accounts/
  models-cache-cn.json   动态模型清单落盘缓存（7 天）
  models-cache-intl.json
  credits-source.json    额度端点候选命中记录
  machine_identity.json  机器身份
  settings.json
  scheduler/state.json
logs/
  qoder-proxy.log
```

## 2. usage.jsonl 行格式

```json
{
  "at": 1760000000.123,
  "iso": "2026-11-15T21:00:00",
  "model": "Qwen3.8-Max",
  "stream": true,
  "elapsed_ms": 4231, "ttft_ms": 812, "gen_ms": 3419,

  "prompt_tokens": 12400, "completion_tokens": 820,
  "reasoning_tokens": 0, "cached_tokens": 11800,
  "cached_reported": true,
  "total_tokens": 13220,
  "credit": 12.4, "original_credits": 0, "billable": true,

  "account": "01a1df2a-...",
  "realm": "cn",
  "key_id": "a1b2c3d4e5f6",
  "reasoning_effort_requested": "medium",
  "reasoning_effort": "medium",
  "realm_reason": "",

  "msgs_sha": "a1b2c3d4e5f6", "n_msgs": 24,
  "sys_src": "top+inline", "n_msg_system": 1,

  "session_affinity": "memory-prefix",
  "upstream_session_id": "uuid",
  "route_rule": "rule-id",

  "tokens_per_sec": 239.8,
  "cache_hit_pct": 95.2,

  "output_content_chars": 820,
  "output_reasoning_chars": 0,
  "output_has_tool_call": false,
  "output_finish_reason": "stop"
}
```

### 两个关键口径

1. **`credit` 读复数 `credits`**（issue #22）：上游给的是 `usage.credits`，
   旧实现只读单数 `credit` 恒为 None，所有记录成 0。
2. **`cached_reported` 区分"真的没命中"和"上游没回报字段"**（issue #129）：
   后者在面板显示 `?`，且**不计入"全价请求"**（按全价估会偏高）。

## 3. 积分折算（`split_credit`）

上游有时不直接给积分，用三段系数从 token 反推：

```
QD_CREDIT_COEF = "非缓存,缓存,输出"     默认 1.069, 0.086, 4.277（每千 token 积分）

input_credit  = (prompt - cached) * a / 1000
cached_credit = cached            * b / 1000
output_credit = completion        * c / 1000
```

默认值来自 244 条 DeepSeek-Flash 实扣 + 110 条 Qwen3.8-Flash 名义的拟合，
残差 < 1%。

## 4. 性能指标（`perf_stats`）

- 采样最近 5000 条（`P50/P90/P99`）：
  - `ttft_ms`（首字延迟）
  - `gen_ms`（生成耗时）
  - `wall_ms`（总耗时）
  - `tokens_per_sec`
- 按模型/区域分组
- 固定 TTL 10 秒的有界缓存；**日志发生追加/截断/替换时立即失效**
- 相同冷查询并发合并（single-flight）
- 缓存统计暴露在 `/health.analytics_cache`（命中/计算/合并等待数）

### 内层错误（HTTP 200 里的错误）

`inner_errors` 字段：

```json
{"total": 7, "kinds": {"invalid_request": 5, "rate_limit": 2},
 "warned": ["invalid_request", "rate_limit"]}
```

## 5. 统计概览（`compute_usage_stats`）

单次扫描 `usage.jsonl` 产出全部面板数据：

### KPI

| 字段 | 说明 |
|---|---|
| `requests` / `success` / `errors` / `success_pct` | 成功率 |
| `tokens` / `prompt` / `completion` / `cached` | token 分解 |
| `active_days` | 窗口内活跃天数 |
| `streak_days` | 连续天数（今天没请求时从昨天开始数） |
| `top_model` | 用量最高的模型 |

### 缓存命中四窗口

`10m` / `1h` / `24h` / `7d` —— 按"现在"回溯，与统计窗口无关。
每项 `{hit, input, pct}`。

### 24 小时曲线

按本地整点聚合，每点 `{ts, label, pct, tokens}`。
`pct` 为 `null` 时表示该小时没有请求（不是 0%）。

### 全年热力图

`heatmap.days` 是逐日 token 数组，`max` 给前端定色阶。
**固定看最近 365 天，与统计窗口无关。**

### 全价请求（issue #129 后续）

```json
"full_price": {
  "requests": 9, "prompt": 1080000, "credit": 42.3,
  "credit_share": 30.1,          // 占窗口内总积分的百分比
  "threshold": 100000            // QD_FULL_PRICE_PROMPT_MIN
}
```

定义：`prompt_tokens >= 100000 && cached_tokens == 0 && cached_reported`。
目的：让用户直接看到"换前缀 / 新开会话"的代价（一天 9 次这类请求可占 30% 积分）。

### 排名

`top_accounts` / `top_providers` / `models` / `providers`，每项
`{name, key, tokens, requests, share}`。账号名优先用 nickname。

`provider_of(model)` 按模型名关键词归类（Qwen / GLM / DeepSeek / Kimi /
MiniMax / Cantus / 其他）。

## 6. 时间窗口语义（`range_window`）

| range | 窗口 |
|---|---|
| `today` | 本地自然日 |
| `week` | 最近 7 天 |
| `month` | 最近 30 天 |
| `custom` | `?since=&until=`（epoch 秒） |
| 空 / `all` | 全部 |

`realm_scope(value)`：字面 `all` → None（无过滤）；其他值原样按字面比较
（未知值过滤出空集，**不替面板猜意图**）。

## 7. 输出上限学习（`learn_max_output_limit`）

官方模型清单里没有 `maxOutputTokens`，但上游对超限请求回：

```
400 invalid_parameter_error: Range of max_tokens should be [1, 131072]
```

网关把这行当成**学习信号**：

```
解析出 N
→ 按 (区域, 模型) 记住并落盘 usage/max-output-limits.json（7 天 TTL）
→ 之后的请求若 max_tokens 超过 N，自动压到 N 并打 WARN
→ /v1/models 补上 max_output_tokens / max_completion_tokens
```

也就是说：**第一次**超限仍会看到上游那条 400（网关要靠它才知道上限），
之后同一模型就不会了。

## 8. 风险遥测（`/health.risk`）

网关启动以来收到的风控类信号次数：

| 字段 | 含义 |
|---|---|
| `queue` | 排队（10605 / isQueued / RESOURCE_LIMIT） |
| `rate` | 429 限流 |
| `rejected` | 403 拒绝 |
| `version_gate` | 430 版本过低 |
| `timeout` | 首字超时 |
| `last` | 最近一次信号 `{kind, at, detail[:120]}` |

**不含凭证**，是只读遥测。

## 9. 环境自检（`/health.env`）

```json
{
  "container": false,          // 是否在容器中
  "proxy": false,              // 是否配置了出口代理
  "accounts_writable": true,   // accounts/ 是否可写
  "identity_cached": true,     // 机器身份是否已缓存
  "clock_skew_s": 1,           // 与上游的时钟偏差（秒）
  "traffic_guard": "auto"      // 流量守卫模式
}
```

`clock_skew_s` 用上游 `Date` 头测（无鉴权、只发 HEAD，结果缓存 10 分钟）。
**时钟漂移会让 COSY 签名直接 401**，这是"容器里莫名失效"的常见原因。

`accounts_writable = false` 意味着设备身份无法持久，容器重建看起来就像换了设备。

## 10. 错误文案归一（`friendly_upstream_error`）

返回 `(message, err_type)`，把上游的原始错误翻译成可执行的中文说明。

| err_type | 触发 | 文案要点 |
|---|---|---|
| `upstream_image_conversion_timeout` | 图片转换超时 | 不是 Key 失效；保留图片、限制重试、不冷却账号 |
| `upstream_queued` | 10605 / isQueued / RESOURCE_LIMIT | 模型排队；换模型/区域；不是 Key/账号/网络问题 |
| `quota_exhausted` | 112 | **不等于账号总余额为零**；核对出口账号与套餐权限 |
| `quota_daily_limit` | 110 | 今日次数上限；本地 0 点后恢复 |
| `version_too_low` | 430 | 客户端/网关版本过低 |
| `payload_too_large` | 本地 413 | 已在本地拦下、未发送 |
| `invalid_tool_arguments` | 参数非法 JSON | 本轮未执行任何工具，请重试 |
| `content_policy_rejected` | DataInspectionFailed 等 | 确定性拒绝、重试无效 |
| `upstream_transient_error` | 418/5xx/provider_error | 已自动重试仍失败，稍后重试 |
| `upstream_incomplete_tool_call` | missing_tool_name / truncated_arguments | 已阻止伪装为正常结束 |
| `upstream_empty_response` | reasoning_only / empty_tool_turn | 未把思考改成正文 |

### 两个安全化处理（issue #137）

1. **`_neutralize_auth_codes`**：把文字里独立出现的 `401`/`403` 改写成
   `HTTP_401`/`HTTP_403`，避免客户端把"上游排队/额度"误判成"我的 Key 失效"。
2. **`_client_error_status`**：非凭证类错误即使上游给 403，也发给客户端 429。

## 11. 错误分类矩阵（重试决策）

`_is_transient_upstream(code, detail)`：

```
401/403/429                    → False（各自有专门路径）
包含客户端错误标记             → False
418 或 >= 500                  → True
400-499 且含 provider_error    → True
其余                           → False
```

客户端错误标记（**永不重试**）：

```
invalid_parameter_error, invalid_request_error, authentication_error,
permission_error, "Range of ", DataInspectionFailed,
inappropriate content, input text data may contain, ContentFilter, SensitiveContent
```

`_is_request_fault(code, detail)`：**换号结果不变**的确定性错误
（413 / invalid_parameter_error / invalid_request_error / `"Range of "` / 内容审核）。
命中时**不冷却、不换号**，直接把原错误抛给客户端 ——
否则轮换只会把全池冷 60 秒，其他用户跟着吃 503。

`_is_transient_transport(exc)`：SSL EOF / reset / timeout / broken pipe /
connection 类异常。

## 12. 定价参考（`qoder_pricing`）

```python
DEFAULT_PRICES = {
    "dfmodel": (0.28, 0.028, 0.42),        # (输入未命中, 输入缓存命中, 输出) USD/1M
    "dmodel":  (0.55, 0.14, 2.19),
    "gmodel":  (0.60, 0.15, 2.20),
    "gfmodel": (0.06, 0.015, 0.40),
    "kmodel":  (0.60, 0.15, 2.50),
    ...
}
```

优先级：`usage/pricing-overrides.json` > `DEFAULT_PRICES` > **未收录则不计价**。

覆盖文件支持两种形态：

```json
{"dfmodel": {"in": 0.28, "cached": 0.028, "out": 0.42},
 "my-custom-model": [0.5, 0.1, 2.0]}
```

**未收录模型显示 `—`，绝不猜数字**。只用于"这些 token 放在 OpenRouter 上大概
值多少钱"的参考，**不能用于结算**。

## 13. 日志

三种出口：

1. `logs/qoder-proxy.log`（`qoder_logpaths.install_stream_logging`）
2. 内存环形缓冲（`/logs` 接口，支持 `level`/`tag`/`search`/`since_id`）
3. stderr

tags：`system` / `chat` / `realm` / `account` / `scheduler` / `image` /
`keepalive` / `usage` / `anthropic` / `webtools` / `credits`

`log_message` 会**静默看板高频轮询的正常 200 GET**（`/logs`、`/usage`、
`/accounts`、`/scheduler`、`/health`、`/panel/status`、`/realm`），
异常状态与业务操作照常记录。`/favicon.ico` 的 404 不刷屏。

## 14. 活动流水（`qoder_activity`）

`usage/activity.jsonl`，一行一个事件：

```json
{"ts": 1760000000, "kind": "checkin", "ok": true, "uid": "01a1df2a",
 "nickname": "...", "realm": "cn", "detail": "...", "amount": 100}
```

- 超过 5 MB 自动轮转为 `.1`
- 读接口只返回最近 30 天
- 同一毫秒写入的事件（Windows 计时精度约 15ms）靠**写入顺序**定先后
  （后写的更新），否则"成功/失败"两条会因时间戳相同而顺序不定
- 任何异常都吞掉（流水不能影响主流程）
