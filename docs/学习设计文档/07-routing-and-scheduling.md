# 07 · 路由、亲和、保温与调度

## 1. 双区路由（三层决策）

### 1.1 第一层：显式指定

优先级（`_request_realm`）：

```
?realm= 参数（作为显式入参传入）      ← open_upstream(target_realm=...)
> API Key 绑定的出口
> X-Realm 请求头
> ?realm= 查询参数
> 全局出口 CURRENT_REALM
```

Key 绑定出口与显式 realm 冲突时直接 400（`qoder_routing.resolve` 抛
`API Key realm conflicts with requested realm`）。

### 1.2 第二层：按模型归属自动选区（`detect_model_realm`）

```
区域独占硬表（exclusive_realm）
  > 清单驱动（auto_realm_for_model：只在"该模型只在一个区域存在"时才换）
  > 全局开关
```

独占表（当前快照）：

| 区域 | 独占 key |
|---|---|
| CN | `q37fmodel`、`gm51model` |
| INTL | `efficient`、`cmodel`、`smodel`、`ultimate`、`performance` |

判据是"**判不出就不猜**"：双区都有的同名模型物理上无法靠名字区分，始终留在当前区。

**不能用账号可用性污染这一层**：`row_matches_realm` 等纯统计路径也调它。

### 1.3 第三层：按账号可用性兜底（`fallback_realm_by_accounts`）

只在**用户没显式指定**时生效：

```
当前区 count_ready > 0        → 不动
当前区 == 0 且 另一区 > 0     → 换区
两区都是 0 / 异常 / 无池      → 不动（交给错误路径，不猜）
```

注意短路：**如果 realm 已经被模型规则推走，就不再按账号换回来** ——
那说明当前区根本没有这个模型，换回去只会打到"没有这个模型"的上游。

### 1.4 换区可见性

- 响应头 `X-Qoder-Realm` / `X-Qoder-Realm-Reason`
- `realm_reason` 取值：
  - `model_missing`：当前区官方清单里没有这个模型
  - `no_account`：模型本区有，是账号可用性把它推走的
  - 空串：没有换区
  - **键不存在**：该记录产生于字段上线前（与"没换区"严格区分）
- 流式响应同样标注（`_sse_begin` 里写头）

## 2. 本地路由规则（`qoder_routing.py`）

存储：`accounts/admin/routing.json`（子目录避免与 `<uid>.json` 冲突）。

```json
{
  "rules": [{
    "id": "...", "name": "未命名规则", "enabled": true,
    "model": "qwen*", "realm": "cn",
    "strategy": "round-robin|weighted-round-robin|fill-first",
    "groups": ["team"], "accounts": ["uid1", "uid2"],
    "key_ids": ["panel-key-id"], "priority": 10
  }],
  "tiers": [{"id":"...", "name":"首选", "accounts":[...], "weights":{"uid1":3}}],
  "policy": {"strategy":"round-robin", "session_affinity":true,
             "request_retry":0, "max_retry_accounts":0},
  "board_enabled": true,
  "updated_at": 1760000000
}
```

### resolve 的合并规则

```
forced  = key.realm or explicit_realm
allowed = scope_uids(key.account_ids, key.account_groups)     # None = 不限制

for rule in rules（按 priority 排序，第一个匹配的生效）:
    跳过未启用 / model fnmatch 不匹配 / key_ids 不含本 key / realm 强制冲突
    allowed &= scope(rule.accounts, rule.groups)               # 只能收窄，绝不扩大
    break

if realm:  allowed &= {该区域的账号}
if tiers:  allowed &= {所有 tier 里的账号}
```

**安全不变量**：规则**永远不能扩大** API Key 的账号范围，只能收窄。

### 三种策略

| 策略 | 行为 |
|---|---|
| `round-robin` | 轮转指针，`rows[cursor % len(rows)]` |
| `weighted-round-robin` | 平滑加权轮询（每次给每个账号加权重，取最大，选中者减总量） |
| `fill-first` | 永远选 `rows[0]`（配合 tier 就是"首选层用完再用备用层"） |

`preview()` 是**无副作用**的：给面板展示"这个模型会被路由到哪些账号"。

### 配置缓存（值得移植）

按 `(st_dev, st_ino, st_size, st_mtime_ns, st_ctime_ns)` 做键：

- 原子保存后立即失效
- 外部编辑在 inode/size/mtime/ctime 变化时失效（**没有 TTL 陈旧窗口**）
- 读的是 `fstat(descriptor)` 而不是可能已被替换的路径
- 只有在"路径仍指向我们读到的那个快照"时才缓存（Windows 的 `stat.ctime`
  是创建时间、`fstat.ctime` 可能是修改时间，两者不可混用）
- `expected_revision` 支持乐观并发控制（`routing_conflict`）

## 3. 会话亲和（两级）

### 3.1 账号亲和（`SessionAffinity`）

```
session_key → uid，TTL 7200 秒，最多 5000 条
```

`session_key` 来源（`extract_session_key`）：

```
X-Conversation-Id / X-Session-Id / X-Request-Id 类头
> body 里的会话字段（conversation_id / session_id / metadata.user_id）
> derive_affinity_key(messages)：首条 user 消息 sha256 前 16 位
```

### 3.2 上游 session_id（前缀会话缓存）

`QD_SESSION_CACHE=off|memory|redis`，默认 off。

作用：相同出站消息前缀 → 复用同一个上游 `session_id`，提高缓存命中。

**存的是什么**：只保存 `HMAC(前缀) → 随机 UUID` 的映射。
不保存对话正文、不保存上游 token、不保存 API Key 明文。

**隔离维度**：API Key 标识 × 上游账号 × 区域 × 模型 × 工具 × 参数 × 客户端会话 ID。

**算法**（`PrefixSessionCache.ASSIGN`，Redis Lua 原子脚本）：

```lua
-- 从长到短尝试所有前缀键，第一个命中的就是最长匹配前缀
local selected = false
local matched = 0
for i = #KEYS, 1, -1 do
    local value = redis.call('GET', KEYS[i])
    if value then selected = value; matched = i; break end
end
if not selected then selected = ARGV[1] end     -- 新的随机 UUID
redis.call('SET', KEYS[1], selected, 'EX', ARGV[2])       -- 稳定会话锚
if #KEYS > 1 then redis.call('SET', KEYS[#KEYS], selected, 'EX', ARGV[2]) end
```

内存后端用同一策略 + LRU（`QD_SESSION_CACHE_MAX_ENTRIES`，默认 4096）。

**前缀至少包含第一条非空 user 消息**：不会仅凭共同 system 合并会话。

**失败回退**：Redis 不可达 / 配置错误 / 缓存值非法 → 退到新的随机 session，
可能冷启动但不阻断推理。日志**不打印连接地址或异常详情**。

**保守约束**：只支持单机或代理入口，不跟随 Cluster 的 MOVED/ASK 重定向。

### 3.3 换号后重新查找

账号故障切换后，要为**新账号**重新查找它自己的映射（不同账号的上游缓存是独立的）。

## 4. 缓存保温（`qoder_keepalive`）

### 问题

上游 prompt 前缀缓存有闲置过期行为，实测约 5 分钟（命中会续期）。
闲置超过该窗口后，下一发请求全量重写：124k prompt ≈ 137 credits（命中时 ≈ 13.8）。

### 机制

成功转发一条真实对话请求后，把客户端原始 payload 按**最终 session_id** 记在内存
（只保留最近 N 个会话，LRU）。后台线程每 20 秒巡检：

```
会话闲置 >= INTERVAL 且 <= MAX_AGE 时
  → 用同 payload 重发一次 stream=false, max_tokens=1 的最小请求
  → 前缀逐字节相同 → 命中缓存并续期
```

### 参数

| 变量 | 默认 | 说明 |
|---|---|---|
| `QD_CACHE_KEEPALIVE` | `0` | 总开关（不开不存任何数据） |
| `QD_CACHE_KEEPALIVE_INTERVAL` | 240 | 闲置多少秒后保温（须 < 上游 TTL） |
| `QD_CACHE_KEEPALIVE_MAX_AGE` | 1800 | 闲置超过该秒数后不再保温 |
| `QD_CACHE_KEEPALIVE_MAX_CONVS` | 2 | 最多登记几个会话的 payload |

### 边界

- 只存 payload 本身，**不落盘**
- 保温请求照常记账，`key_id = "keepalive"`，面板可见可审计
- 上游对"同会话高频重复请求"的风控容忍度未知：从小 INTERVAL、少会话起步，
  观察 `/health` 的 `risk` 遥测

## 5. 流量守卫（团队账号保护）

限制单账号并发与请求节奏，避免触发排队或风控。

| 变量 | 默认 | 说明 |
|---|---|---|
| `QD_TRAFFIC_GUARD` | 未设置 | `on` 全部启用 / `off` 全关 / 未设由 `QD_TEAM_GUARD` 决定 |
| `QD_TEAM_GUARD` | `auto` | `auto` 只对识别为团队/企业的账号启用 |
| `QD_TEAM_MAX_CONCURRENCY` | 1 | 单账号同时进行的请求数 |
| `QD_TEAM_MIN_GAP_MS` | 400 | 同账号两次请求启动的最小间隔（另加 0–120ms 抖动） |
| `QD_TEAM_WAIT_SECS` | 30 | 拿不到并发名额最多等多久（超时后放行并 WARN，不卡死） |
| `QD_TEAM_MAX_CLIENTS` | 3 | 一小时内同账号来自 >N 个客户端 IP 时 WARN |

团队账号判定（`_is_team_account`）：

1. userinfo 里带 `organization_id` / `organization_name`
2. `userType` 含 `team` / `enterprise`

`_TeamLeaseResp` 是个包装器：`with resp` 退出时自动释放租约，
handler 都走 `with resp` / `resp.close()`，所以不会泄漏名额。

## 6. 调度器（`qoder_scheduler`）

### 6.1 排程配置

```json
{
  "enabled": true,
  "checkin_times": "09:00,21:00",
  "keepalive_times": "22:00",
  "cron": "",
  "jitter_secs": 8
}
```

- `checkin_times` / `keepalive_times`：逗号分隔的 `HH:MM`，非法项忽略
- `cron`：5 段表达式，**覆盖 checkin_times**（保活仍按 keepalive_times）
  - 支持 `*` / `a-b` / `a,b` / `*/n` / `a-b/n`
- `jitter_secs`：账号之间的随机间隔上限（0–60）

### 6.2 关键设计：到点即补，而不是整点轮询

旧实现靠"每分钟轮询 `cur_min == 0`"，有两个必漏点：

1. 容器时区与宿主机不同 → 命中的是 UTC 的 9 点，不是用户本地 9 点
2. 上一轮巡检跑超过 1 分钟 → 整点那一分钟整个跳过，当天就没了

新实现：每 15 秒检查"有没有**已过点但还没触发**的排程"，**最多回补 3 小时**
内的漏点（进程重启也接得上）。

### 6.3 已触发点的持久化（issue #130）

`_fired` 是 `{ts: True}` 的 map，**落到状态文件里**：

- 重启会把 3 小时内的排程点再跑一遍（实测出现两次签到相隔 3 秒）
- 落盘后：`_fired_export` 只导出近 3 天的，`_fired_import` 只恢复
  "过去 3 天内且不在未来"的值（防脏数据锁死排程）

**改排程时不清空 `_fired`**：清了会让 3 小时内的排程点再触发一次（重复签到）。
新排程点有新的 ts，天然不会被误挡。

### 6.4 启动补签闸门

启动巡检会对 `can_checkin()` 为真的账号发真实领取请求，进程反复重启就会反复领取。

`_allow_complement_checkin`：**同一天最多补签一次**，且是
**mark-before-act**（先把当日标记落盘，再去做领取）——即使随后进程崩溃，
重启后也不再重放。

第二道闸：`can_checkin()` 依赖 per-account `lastCheckin`（落盘在账号 JSON）。

### 6.5 一次巡检的完整流程

```
1. Token 保活
   22:00 全量刷新（force=True）；其余巡检只刷新剩余寿命 < 4 小时的
   收集 failed_uids

2. 计算 stale_uids（本轮失效凭证账号）
   = 保活明确失败 ∪ token 已过期
   这些账号**整体排除**在后续步骤之外
   理由：跳过只是"不再打上游"，但一轮里若已用失效 token 拿到退化响应，
        那次响应会写进账本（把健康余额覆盖成 0）

3. 每日签到
   签到窗口内（09:00/21:00）：targets 全量
   其他巡检：只补签 can_checkin() 为真的账号（受启动闸门限制）
   禁用账号只要开了"暂停期间仍签到"也进 targets

4. 刷新额度快照（每个账号 fetch_credits，间隔 0.5 秒）
```

### 6.6 状态持久化

`<accounts>/scheduler/state.json`（子目录，避免被 `AccountPool.load()` 当成账号）：

```json
{
  "enabled": true, "checkin_times": "...", "keepalive_times": "...",
  "cron": "", "jitter_secs": 8,
  "last_run_time": "2026-11-15 21:00:03",
  "next_run_time": "2026-11-16 09:00:00",
  "startup_claim_date": "2026-11-15",
  "last_cycle_date": "2026-11-15",
  "logs": ["[2026-11-15 21:00:03] 开始执行任务..."],
  "fired": [1760000000, ...],
  "updated_at": "..."
}
```

`_save_state` **不得调用 `self.log()`**，否则与 `log → _save_state` 形成递归。

## 7. Go 移植要点

| 组件 | Go 方案 |
|---|---|
| 调度器 | `time.Timer` + 15 秒 ticker；`_next_target()` 逻辑照搬（含 3 小时回补窗口） |
| 已触发点 | `map[int64]bool` + 落盘 JSON，导出/导入时做范围过滤 |
| 会话亲和 | `sync.Map` + 惰性过期，或 LRU（`container/list` + map） |
| 前缀会话缓存 | 内存后端用 LRU；Redis 后端用 RESP2 手写客户端（~200 行）或引入 `go-redis`（但项目原则是零依赖，建议手写） |
| 路由配置缓存 | `stat` 的 `(dev, ino, size, mtime, ctime)` 做键；Go 的 `os.File.Stat()` 与 `os.Stat()` 语义差异需注意 |
| 流量守卫 | `chan struct{}` 信号量 + `time.Ticker` 控制最小间隔；租约用 `defer release()` |
| 保温线程 | `time.NewTicker(20s)` + `context.Context` 取消 |
