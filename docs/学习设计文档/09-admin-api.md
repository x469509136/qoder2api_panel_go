# 09 · HTTP 端点全表

鉴权标记：**公开** = 无需凭证；**Key** = API Key（未配置任何 Key 时也可访问，
面板会话同样放行）；**面板** = `X-Panel-Token`。

## 1. 推理

| 方法 | 路径 | 鉴权 | 说明 |
|---|---|---|---|
| POST | `/v1/chat/completions`、`/chat/completions`、`/v1/completions` | Key | Chat Completions |
| POST | `/v1/responses`、`/responses` | Key | Responses API |
| POST | `/v1/messages` | Key | Anthropic Messages |
| POST | `/v1/messages/count_tokens` | Key | 本地估算，不请求上游 |
| GET | `/v1/models`、`/models` | Key | 模型清单，`?merge_realms=1` 合并双区 |
| GET | `/models/refresh`、`/v1/models/refresh` | Key | 立即重新拉取模型清单 |

请求头：`X-Realm`、`X-QD-Context`、`X-Conversation-Id`。
响应头：`X-Qoder-Realm`、`X-Qoder-Realm-Reason`。

## 2. 探活与状态

| 方法 | 路径 | 鉴权 | 说明 |
|---|---|---|---|
| GET | `/ping`、`/healthz`、`/livez`、`/readyz` | 公开 | 纯文本 `pong`，不查账号池 |
| GET | `/health` | 公开 | 版本、区域、账号数、`risk`、`env`、`quality`、`image_preflight`、`analytics_cache`；带 Key 时额外返回当前账号 |
| GET | `/realm` | 公开 | 当前出口与自动选区开关 |
| POST | `/realm` | Key | 切换全局出口 |

## 3. 看板与面板

| 方法 | 路径 | 鉴权 | 说明 |
|---|---|---|---|
| GET | `/`、`/dashboard`、`/ui` | 公开 | 单文件看板 |
| GET | `/panel/status` | 公开 | `{panel_password_required, panel_password_is_default, authenticated, api_key_set}` |
| POST | `/panel/login` | 公开 | 返回 token |
| POST | `/panel/logout` | 面板 | 换密钥，所有旧 token 失效 |
| POST | `/panel/password` | 面板 | 改密码（同时踢出所有会话） |
| GET | `/i18n/manifest` | 公开 | 语言包清单 |
| GET | `/i18n/<pack>` | 公开 | 语言包 |

## 4. 设置

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/settings` | 读取全部设置（Key 打码） |
| POST | `/settings/save` | 保存（API Key、限额、模型闸门等） |
| GET | `/settings/reveal?id=` | 读取某个 Key 明文；**面板还在用默认口令时 403** |
| POST | `/settings/tool-echo-mode` | `strict` / `lenient` |
| POST | `/settings/anthropic-thinking` | `empty` / `off` |
| POST | `/settings/model-context` | 单模型上下文档（1M/400K/200K/数字） |
| POST | `/settings/web-tools` | 本地 web 工具开关 |

## 5. 账号

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/accounts` | 账号列表 |
| POST | `/accounts/login/start` | 开始 OAuth 设备授权，返回 `authUrl` |
| GET | `/accounts/login/poll?state=` | 轮询授权结果 |
| POST | `/accounts/login/cancel` | 取消 |
| POST | `/accounts/import/pat` | 导入 PAT |
| POST | `/accounts/import/desktop` | `{}` 只扫描；`{"path":...}` 导入指定；`{"all":true}` 全部导入 |
| GET | `/accounts/export` | JSON 导出 |
| POST | `/accounts/import` | JSON 导入（数组，也支持 dry-run 预检） |
| POST | `/accounts/refresh` | 刷新 Token |
| POST | `/accounts/test` | 测试账号（默认用免费模型探活） |
| POST | `/accounts/set` | 修改单个（enabled / vpcEndpoint / group / note） |
| POST | `/accounts/set-all` | 批量修改 |
| POST | `/accounts/delete` | 删除 |
| POST | `/accounts/note` | 备注（纯展示） |
| GET / POST | `/accounts/credits` | 逐个账号**向上游**查余额（不要轮询） |
| GET | `/credits/summary` | 本地记录的余额汇总（**不请求上游**） |
| GET | `/accounts/export` | 导出 |
| POST | `/accounts/checkin` | 每日签到（单个或全部） |
| POST | `/accounts/checkin-when-disabled` | 设置"暂停期间仍签到" |

## 6. 额度洞察

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/credits/probe` | 探测额度端点候选矩阵 |
| GET | `/accounts/credits/grants` | 积分包/获取记录（最近快照，**不发上游**） |
| GET | `/accounts/credits/usage` | 官方积分消耗（上游 `credits-summary` + `credits-heatmap`，每账号缓存 5 分钟） |
| GET | `/accounts/activity` | 签到/领取动作流水（30 天） |
| GET | `/usage/remaining` | 每账号剩余积分 + 近 24h 消耗 + 预计可用天数 |
| GET | `/pricing` | 参考价目表 + 未定价模型清单 |

## 7. 任务与调度

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/tasks` | 签到状态、活动、福利包资格、额度快照；`?realm=` |
| POST | `/tasks/run` | 批量签到并领取活动奖励 |
| POST | `/tasks/travel` | 批量领取 Pro 福利包 |
| GET | `/scheduler` | 调度器状态与排程日志 |
| POST | `/scheduler/trigger` | 立即执行一次巡检 |
| POST | `/scheduler/toggle` | 开关调度器 |
| POST | `/scheduler/config` | 修改排程（时刻 / cron / jitter） |

## 8. 用量

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/usage`、`/v1/usage` | 用量汇总 |
| GET | `/usage/recent` | 最近请求（分页） |
| GET | `/usage/stats` | 统计概览（KPI/热力图/24h 曲线/全价请求） |
| GET | `/usage/analytics` | 详细透视 |
| GET | `/usage/by-account` | 按账号 |
| GET | `/usage/by-key` | 按 Key |
| GET | `/usage/perf` | 性能指标（含 `inner_errors`） |
| GET | `/usage/timeseries` | 时间序列 |
| GET | `/usage/daily` | 每日用量（`?days=7\|30&group=model\|realm\|key\|account`） |

时间窗口：`?range=day|today|week|month|all|custom&since=&until=`。

## 9. 日志

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/logs` | `?limit&level&tag&search&since_id` |
| GET | `/logs/export` | 下载 |
| POST | `/logs/clear` | 清空 |

## 10. 诊断

| 方法 | 路径 | 鉴权 | 说明 |
|---|---|---|---|
| GET | `/diag/vm` | 面板 | 本机虚拟化检测（官方风控桥 + 本机交叉校验） |
| POST | `/diag/queue` | 面板 | 探测某模型此刻是否在排队 |
| POST | `/diag/checkin` | 面板 | 签到诊断 |

`/diag/queue` 返回：

| 字段 | 含义 |
|---|---|
| `kind` | `ok` / `queued` / `exhausted` / `daily` / `error` |
| `code` | 上游 HTTP 状态 |
| `retry_after` / `queue_count` | 上游给的等待秒数与队列长度 |
| `identity_source` | `native`（官方风控桥）/ `derived`（派生身份） |
| `advice` | 一句话结论 + 可执行建议 |

## 11. 更新

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/update/check` | 检查 GitHub 新版本（缓存 6h，`force=1` 刷新） |
| GET | `/update/plan` | 一键更新前预检（是否容器 / 是否被禁用 / 是否本机 / 是否有更新），返回 `can_apply` 与 `apply_blocked_reasons` |
| POST | `/update/apply` | 执行更新 |

## 12. 智能体配置

| 方法 | 路径 | 鉴权 | 说明 |
|---|---|---|---|
| GET | `/agents` | 面板 | 本机客户端检测结果与接入状态 |
| GET | `/agents/models` | 面板 | 用于预填的模型表 |
| GET | `/agents/template?client=&base_url=&model=` | 公开 | 某客户端的配置模板 |
| GET | `/agents/cli` | 公开 | 配置器 CLI 源码（远程机器 curl 取用） |
| POST | `/agents/apply` | 面板 + **仅本机** | 写入客户端配置 |
| POST | `/agents/restore` | 面板 + **仅本机** | 还原到接入前 |

## 13. 管理面（路由工作台）

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/admin/overview` | 总览 |
| GET | `/admin/groups` | 账号组 |
| POST | `/admin/accounts/group` | 设置账号分组 |
| POST | `/admin/groups/action` | 组操作 |
| GET | `/admin/routing` | 路由配置（rules / tiers / policy / revision） |
| POST | `/admin/routing/save` | 保存（支持 `expected_revision` 乐观并发） |
| POST | `/admin/routing/preview` | 无副作用预览 |
| GET | `/admin/jobs` | 后台任务状态 |

## 14. 支持的客户端（`/agents`）

**13 类支持自动配置与还原**：Claude Code、Codex CLI、OpenCode、DSH、Crush、
Aider、Continue、Pi、Factory Droid、GitHub Copilot CLI、OpenHands CLI、
Kilo Code CLI、Mistral Vibe。

**18 类只做检测与手动指引**：Goose、Gemini CLI、Qwen Code、Amp、Cline、
Roo Code、Kilo Code (VS Code)、Cursor、Windsurf、Trae、Zed、Cline CLI、
JetBrains Junie、Augment Code、Cherry Studio、Chatbox、LobeChat、Open WebUI。

### 配置写入的两种编辑策略

| 形态 | 策略 | 例子 |
|---|---|---|
| JSON | `deep_merge` 后整文件写入 | Pi `models.json`、Kilo `kilo.json`、OpenHands |
| YAML | 行级 upsert（保留注释与其他设置） | Aider `.aider.conf.yml`、DSH `settings.yaml` |
| TOML | 行级 upsert 顶层键 / 表 | Codex `config.toml`、Vibe `config.toml` |
| 托管块 | 用 `# qoder-proxy:<name>:begin/end` 包住的片段 | Continue、Vibe |
| 保守拒绝 | 遇到 JSONC 带注释、非块列表等复杂结构时**拒绝改写**并给手动方案 | Kilo JSONC、Continue 非块 models |

### 备份与还原

- 首次 apply 时**备份每个被碰文件的原件字节**
- 后续 apply 保留**最早那份**备份（restore 永远回到**第一次接入前**）
- 多文件客户端**两阶段写入**：任一文件失败，已写文件整体回滚
- `_prune_backups(directory, keep=BACKUP_KEEP)` 控制备份份数

### 安全边界

`/agents/apply` 与 `/agents/restore` **只信 loopback**（改的是网关本机的文件）。

## 15. 本地 web 工具（`QD_WEB_TOOLS`）

Codex 等客户端在 Responses 请求里声明的内置工具 `web_search` / `web_fetch`，
Qoder 上游不会执行；开启后由**网关本地执行**：

```
客户端声明 type=web_search/web_fetch
  → 网关改写成同名 function 声明
  → 模型调用该 function
  → 网关本地搜索/抓页面
  → 结果回灌给模型继续生成（最多 3 轮）
```

### 安全边界（这是唯一会由**模型**驱动出网的地方）

- 只允许 http/https，禁止带凭据的 URL
- 目标域名解析后必须是**全球可路由单播**地址（`ip.is_global`）
- **解析结果钉死到实际连接**（防 DNS 重绑定：check 一次、connect 再解析一次是漏洞）
- 重定向**逐跳复检**，重定向到内网直接失败
- 不信任环境代理：本地 web 工具一律直连钉好的 IP
  （要代理请显式给 `QD_WEB_TOOLS_PROXY`，同样受地址校验）
- 响应体 4 MB / 正文 20000 字上限，超时 15 秒

### 引擎链与限流

`duckduckgo` → `bing` → `wikipedia`，TTL 缓存 600 秒，
被限流冷却 600 秒、其他错误冷却 60 秒，引擎间最小间隔 0.5 秒。

### 开关与代价

| 开关 | 说明 |
|---|---|
| 面板「设置 → 本地 web 工具」 | 持久化 `settings.json` 的 `web_tools` |
| `QD_WEB_TOOLS=on/off` | 环境变量优先于面板 |
| `QD_WEB_TOOLS_ROUNDS` | 代跑轮次上限，默认 3 |

每代跑一轮都会多消耗一次账号额度；默认关闭。

## 16. CORS

`cors_origin_allowed(path)`：只对推理与管理路径回 `Access-Control-Allow-Origin: *`，
`OPTIONS` 返回 204 并放开 `Allow-Headers/Methods`。

## 17. 安全提示摘要

| 风险 | 防护 |
|---|---|
| 面板默认口令 `admin` | 启动时 WARN；面板提示改密；默认口令下不显示明文 Key |
| LAN 模式无 Key | `--lan` 强制生成并持久化随机 Key |
| settings.json 被写坏 | 写前确认可解析，否则拒绝回写 |
| 面板 token 泄露 | 只在 `X-Panel-Token` 头，不进 query（避免历史/Referer/代理日志） |
| web 工具 SSRF | 全链路地址校验 + 钉 IP + 逐跳重定向复检 |
| 智能体配置越权 | 只接受 loopback |
| 上游凭证外泄 | usage 行只记 `key_id`（面板 id），绝不含明文 |
