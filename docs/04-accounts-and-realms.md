# 04 · 账号、区域与凭证生命周期

## 1. 双区常量表（`REALM_CONFIGS`）

| 字段 | cn | intl |
|---|---|---|
| openapi | `https://openapi.qoder.com.cn` | `https://openapi.qoder.sh` |
| gateway | `https://gateway.qoder.com.cn` | `https://api3.qoder.sh` |
| gateway_fallbacks | — | `api2.qoder.sh` → `api1.qoder.sh` |
| website | `https://qoder.com.cn` | `https://qoder.com` |
| domain | `qoder.com.cn` | `qoder.com` |
| client_id | `1c5e33e1-364d-4ce6-b02c-acaa81274a5c` | `e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb` |
| redirect_uri | `qoder-work-cn://` | `qoder://aicoding.aicoding-agent/login-success` |
| ua（账号接口） | `QoderWork/1.1.75` | `Qoder/1.1.75` |
| nonce 形态 | 带横线 UUID | 32 位 hex |
| home_dir | `.qoder-cn` | `.qoder` |
| app_dir | `com.qodercn.app.stable` | `com.qoder.app.stable` |

`send_client_id` / `send_redirect_uri` 控制设备授权 URL 带哪些参数。

### 业务端点（挂 openapi，纯 Bearer，无 COSY）

```
PATH_DEVICE_POLL       /api/v1/deviceToken/poll
PATH_DEVICE_REFRESH    /api/v1/deviceToken/refresh
PATH_JOB_EXCHANGE      /api/v1/jobToken/exchange
PATH_JOB_REFRESH       /api/v1/jobToken/refresh
PATH_USERINFO          /api/v1/userinfo
PATH_QUOTA             /api/v2/quota/usage
PATH_PLAN              /api/v2/user/plan
PATH_CHECKIN_STATUS    /sash/api/v1/me/daily-check-in/status
PATH_CHECKIN_CLAIM     /sash/api/v1/me/daily-check-in/claim
PATH_PRO_ELIGIBILITY   /sash/api/v1/me/pro-upgrade/eligibility
PATH_PRO_CLAIM         /sash/api/v1/me/pro-upgrade/claim
PATH_CAMPAIGNS         /sash/api/v1/me/campaigns
PATH_CAMPAIGN_REWARD   /sash/api/v1/me/campaigns/{id}/reward
PATH_CAMPAIGN_CLAIM    /sash/api/v1/me/campaigns/{id}/claim
PATH_CREDIT_SUMMARY    /sash/api/v1/ai-conversations/credits-summary
PATH_CREDIT_HEATMAP    /sash/api/v1/ai-conversations/credits-heatmap
PATH_ACHIEVEMENTS      /sash/api/v1/me/achievements
```

### 企业 VPC（仅 CN）

`QD_CN_VPC_ENDPOINT` 可以是实例名 `acme`，或 `acme.vpc.qoder.com.cn` /
`acme-openapi.vpc.qoder.com.cn` / `acme-gateway.vpc.qoder.com.cn`。
解析后按账号推导 openapi / gateway / website / domain：

```
acme                 →  acme.vpc.qoder.com.cn 家族
```

三条硬规则：

1. 账号显式 `vpcEndpoint` > 凭证域推断 > 部署默认 > 公网
2. **VPC 不回退公网**（不能把企业凭据先送往公网）
3. VPC 只对 CN 生效，intl 账号设了会抛错

## 2. `Account` 对象

### 持久化字段（`accounts/<uid>.json`）

```json
{
  "uid": "...", "nickname": "...", "group": "", "note": "",
  "domain": "qoder.com.cn", "realm": "cn", "vpcEndpoint": null,
  "platform": "CLI",
  "accessToken": "...", "refreshToken": "...", "personalToken": "...",
  "expiresAt": 1790000000, "addedAt": 1760000000, "source": "oauth",
  "enabled": true, "lastError": "", "cooldownUntil": 0,
  "credits": {...}, "plan": "...", "lastCheckin": "2026-11-15",
  "lastCreditsDelta": 100, "lastCreditsDeltaAt": 1760000000,
  "checkinWhenDisabled": false,
  "campaignCodes": {"campaign_key": "REDEMPTION_CODE"},
  "campaignBlockedUntil": {"campaign_key": 1790000000},
  "userType": "personal_professional_trial",
  "organizationId": "", "organizationName": ""
}
```

**不落盘**的运行态（重启重置）：团队租约、冷却计数、三族失败计数、余额刷新状态。

### 凭证族与刷新路由

| token 前缀 | 家族 | 刷新端点 |
|---|---|---|
| `dt-` / `drt-` | deviceToken（OAuth 设备授权） | `/api/v1/deviceToken/refresh` |
| `jt-` / `jrt-` | jobToken（PAT 交换） | `/api/v1/jobToken/refresh`，失败回落 PAT 重新交换 |
| `pt-` | PAT（长期） | 交换成 jobToken |

### 有效期与刷新触发（`ready()`）

```
exp 为空                    → 直接用
remaining > 240 秒          → 直接用（jt- 24h / dt- 30d）
0 < remaining ≤ 240 秒      → 先 refresh 再用
remaining ≤ 0               → refresh（失败即不可用）
```

### `ready(model)` 的完整判据

```
enabled && access_token
&& cooldown_until ≤ now
&& model_cooldowns[model] ≤ now
&& balance_until ≤ now           # 402 停放（UTC+8 每日 10:00 解封）
&& breaker_until ≤ now           # 硬错误熔断
&& !reserve_blocked()            # G1 余额地板
&& !daily_limit_blocked()        # G2 当日 token 总额
&& !credit_limit_blocked(model)  # G3 当日 credit（免费模型豁免）
&& !model_token_limit_blocked()  # G4 单模型当日 token
```

## 3. 四条限额护栏（task-61）

| 键 | 单位 | 语义 |
|---|---|---|
| `reserve_credits` | 积分 | 余额 ≤ 该值即停（含等号） |
| `daily_token_limit` | token | 账号当日累计 token 达到即停 |
| `daily_credit_limit` | 积分 | 账号当日累计消耗达到即停（**免费模型不计入**） |
| `model_daily_token_limit` | token | 单模型当日 token 达到即只停该模型 |
| `expiring_window_days` | 天 | 余额 N 天内到期的账号**优先派发**（不拦截） |

三层作用域：`global` / `intl` / `cn`。区域留空 = 继承 global，填 0 = 该区域关闭。

**fail-open 原则**：计数为 `None`（未折叠）时一律放行，不能因为"不知道"就拦请求。

**共享池例外**（issue #39/#67）：团队账号"套餐 remain=0 但共享池仍有余额"时
按 `remain_effective`（= 套餐 remain + 可用共享池 remain）判定。
共享池 `cap = -1`（不设上限）时 G1 地板护栏**显式跳过**，不靠"数字很大所以没触发"。

## 4. 冷却与失败治理（三族）

### 4.1 一次性冷却（`note_error`）

```
普通：cooldown_until = now + cooldown
单账号池：压到 3 秒（single_account=True）
模型粒度：model_cooldowns[model] = now + wait
```

冷却剩余不超过 10 秒时，后续请求**等待**而不是直接报错（`_short_error_cooldown_wait`）。

### 4.2 三族失败治理（task-74）

| 族 | 触发 | 退避 | 是否挡 `ready()` |
|---|---|---|---|
| 软限流 `note_soft_rate` | 连续 429 | `min(600 * 2^(n-1), 7200)` 秒 | 是（写 `cooldown_until`） |
| 硬错误熔断 `note_failure` | 连续失败 ≥ 3 次 | `min(1800 * 2^(n-3), 21600)` 秒 | **是**（写 `breaker_until`） |
| 未知错误降权 `note_unknown_failure` | 连续未知失败 ≥ N 次 | 10 分钟起 | **否**（只影响选号优先级） |

**统一复位**：`note_success()` 清零 `soft_streak` / `fails` / `degrade_count` /
`breaker_until` / `degrade_until` / `last_error` / `cooldown_until`，
并清掉该模型的冷却。

**刻意不动 `balance_until`**：402 停放只由 `revive_balance_cooldown()` 在余额刷新
看到 `remain > 0` 时解除，两套机制不合并。

**延迟落账（`blame`）**：`note_failure` / `note_unknown_failure` 先记进
`blame` 列表，**等到同一请求换号成功后才真正落账**。全部账号都同样失败时
（本机断网、上游整体故障、超大包被 RST），逐个记账会把全池误打进 30 分钟熔断，
网络恢复也解不开。

### 4.3 402 配额停放

`note_balance_cooled()`：停到**本仓的额度重置窗口**（UTC+8 每日 10:00）。
短冷却无意义——额度每日重置，几分钟后放出去只会再撞一次 402。

## 5. 账号池（`AccountPool`）

### 选号路径

```
pick(realm, exclude, model, allowed_uids)
  ├─ expiring_first（开启临期优先后）→ 余额最快到期的先发
  └─ 否则轮询：
       _rotate_pick：两轮扫描
         第一轮跳过降权中（degraded）的账号
         第二轮才回落使用它们（不楔死）
       每轮内按轮转指针选
```

### 会话亲和（`SessionAffinity`）

```
session_key --(ttl=7200, max_entries=5000)--> uid
```

`derive_affinity_key(messages)`：取**首条 user 消息**做 sha256 前 16 位。

> 为什么只取首条 user 而不是 system+user：system 里常带每轮变化的运行时上下文
> （时间、工具清单、skills、context 用量）。一旦把它算进亲和键，长会话的键会
> 逐轮变化 → 账号轮换 → 上游缓存整段丢失（面板上 98% ↔ 0% 来回跳，issue #136）。

`QD_AFFINITY_MAX_MSGS`：对话超过 N 条后不再固定账号（`0` = 不限制）。

### 换号时解绑亲和

以下情况会 `affinity.unbind(session_key)`：

- 401 / 403 凭证被拒（**但排队 10605 排除在外**——排队是模型级的，换号不会变快，
  反而造成"同会话在多账号间跳"的异常画像）
- 429 限流
- 瞬态上游故障
- 其他 4xx
- 传输层故障

## 6. OAuth 设备授权（PKCE S256）

### 开始（`start_login`）

```
verifier, challenge = PKCE_S256()
nonce = uuid4().hex 或 str(uuid4())      # 按区域
q = {challenge, challenge_method:"S256", nonce}
if send_redirect_uri: q[redirect_uri]
if send_client_id:    q[client_id], q[machine_id]
auth_url = website + "/device/selectAccounts?" + urlencode(q)
state = "qd-" + time_ns
```

`machine_id` 优先读本机官方客户端的 `<auth_dir>/machine_id`（保持设备一致），
缺则新生成。

### 轮询（`poll_login`）

```
GET openapi /api/v1/deviceToken/poll?nonce=&verifier=&challenge_method=S256
  headers: Accept: application/json, User-Agent: QoderWork   ← 不带凭证
```

| 上游响应 | 归一化 |
|---|---|
| 404 / 202 | `pending`（用户还没授权） |
| 任意异常 | `pending`（继续轮询） |
| 有 `token`/`device_token` | 拉 userinfo 补昵称/用户类型/组织，入库 |

登录窗口 600 秒（`LOGIN_TTL_SECONDS`）。`cancel_login` 从表里删掉 state，
若 poll 正在收尾会发现 `self.logins.get(state) is not info` 并返回 `cancelled`。

## 7. PAT 导入

```
POST openapi /api/v1/jobToken/exchange  {"personal_token": "pt-..."}  → token
GET  openapi /api/v1/userinfo           Authorization: Bearer <token>
→ Account(source="pat", personalToken=pat, accessToken=token, refreshToken=rrt-)
```

PAT 必须 `pt-` 开头。jobToken 刷新失败时**回落 PAT 重新交换**，这是 PAT 路径
比 OAuth 更耐用的原因。

## 8. 本机凭证扫描（`scan_desktop_credentials`）

从已装好的官方客户端里读凭证：

| 来源 | 路径 |
|---|---|
| CN CLI | `~/.qoder-cn/.auth/` |
| INTL CLI | `~/.qoder/.auth/` |
| CN VPC | `~/.qoder-cn/.vpc/<instance>/.auth/` |
| CN 桌面 | `%APPDATA%/com.qodercn.app.stable/` |
| INTL 桌面 | `%APPDATA%/com.qoder.app.stable/` |

桌面端凭证是加密的：

```
Local State → os_crypt.encrypted_key（DPAPI 保护）→ AES-256 对称密钥
auth.v1.dat → "v10" + nonce(12) + AES-256-GCM
```

启动时**永不静默导入**：只报告扫描结果，必须用户在看板点确认。

## 9. 额度查询（`fetch_credits`）

### 端点候选矩阵（issue #141）

CN 走 `openapi/api/v2/quota/usage`；INTL 上这个路径常 404/403，
需要用 `/sash/api/v1/me/*`。16 个候选路径按顺序试，第一个解析出数字的**记住**：

```json
// accounts/credits-source.json
{"intl": {"host": "...", "path": "/sash/api/v1/me/credits", "fields": [...], "at": 1760000000}}
```

`POST /credits/probe` 可手动探测。

### 额度归一（`normalize_quota_payload`）

拆成四类容器（认不出就不显示、不臆造）：

| 列 | 来源字段 |
|---|---|
| 套餐内 | `userQuota` / `planQuota` / `subscriptionQuota` |
| 资源包 | `addOnQuota` / `resources` / `packages[]` |
| SOTA | `sotaCredits` / `sota` / `sotaQuota` |
| 共享池 | `orgResourcePackage` / `sharedQuota` / `teamQuota`（组织级，汇总去重） |

每类带 `remain` / `used` / `size`，有 `expiresAt` 就显示最近到期。

### 退化响应识别（#25）

上游对失效凭证会回一个**退化 quota body**：全 0 + `isQuotaExceeded=true` +
`expiresAt=253402214400`（CN 永不过期占位）。

`ledger_is_unknown()` 把它标成"未知"而不是"0"：

- 防止把未知当 0 参与计算（会误触发护栏、误拦账号）
- 面板显示未知，等下次成功刷新再恢复

**真正的"不覆盖健康快照"由 `fetch_credits` 侧守卫**，本层不做回写。

## 10. 签到与活动（`qoder_tasks`）

### 官方规则

- 每日 10:00（UTC+8）刷新，错过不补
- 时间必须按**固定 UTC+8** 计算与呈现，不能用系统本地时区（容器常是 UTC，
  按本地渲染会让"下次签到"与官方说明差 8 小时）

```python
CHECKIN_WINDOW_HOUR_UTC8 = 10
_UTC8 = timezone(timedelta(hours=8))

def next_checkin_window(now):
    # 今天 10:00 之前 → 今天 10:00；否则明天 10:00
    # 返回 (epoch, "10-05 10:00（UTC+8）")
```

### 活动平台（当前主路径）

```
GET  /sash/api/v1/me/campaigns            → 活动列表
GET  /sash/api/v1/me/campaigns/{id}/reward
POST /sash/api/v1/me/campaigns/{id}/claim  （幂等：已领取返回 replayed:true）
```

失败码中文说明：

| 码 | 文案 |
|---|---|
| `REDEMPTION_CODE_OUT_OF_STOCK` | 今日名额已发完（每日 10:00 刷新，次日再来） |
| `ACHIEVEMENT_NOT_COMPLETED` | 需先完成新人任务（成就未完成） |
| `CAMPAIGN_NOT_ACTIVE` | 活动已结束/未开始 |
| `RISK_BLOCKED` | 风控拦截（当前设备/账号不可领取） |
| `RISK_DEPENDENCY_UNAVAILABLE` | 风控服务不可用，稍后重试 |

**领取间隔 ≥ 1.0 秒**（`CLAIM_GAP_DEFAULT`）是硬约束，防风控。

**兑换码必须落盘**（`campaignCodes`）：活动只发一次，不持久化的话重启后用户
就找不回码了。

**"同人已领取"冷却**（`campaignBlockedUntil`）：服务端按人去重（同机器/同身份
多账号共用一张），被挡后反复重试没有意义，只会刷日志。

### 签到能力运行时探测

不用区域硬编码（`has_checkin` 只是提示位）：实际检查端点是否 404，
结果缓存在 `Account._checkin_cap`。

### 签到与推理解耦（issue #102）

`inference_eligible()`（能否接推理）和 `checkin_eligible()`（能否签到）分开：

```
inference_eligible = enabled && access_token
checkin_eligible   = access_token && (enabled || checkinWhenDisabled)
```

禁用账号默认全停，但可以显式打开"暂停期间仍签到"（账号级白拿的每日奖励）。

## 11. 桌面端请求头（活动平台必需）

```
Cosy-ClientType: 10          （CLI=5 / QoderWork=6 / 桌面端=10）
Cosy-Version: <desktop_version>   （默认 0.4.8，QD_DESKTOP_VERSION 可覆盖）
Accept: application/json
User-Agent: Qoder
Authorization: Bearer <access_token>
Cosy-Machine*: ...           （仅在有真实机器身份时）
```

主机名规整规则逐字复刻官方：可打印 ASCII 原样用 → 否则 IDNA → 否则非法字符折成
`-` 并加 sha256 前 8 位；超过 96 字符截断加哈希。
