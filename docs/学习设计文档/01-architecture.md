# 01 · 架构与启动

## 1. 项目定位

把 Qoder 的两个区域（国内版 `qoder.com.cn`、国际版 `qoder.com`）的原生 agent
服务，包装成三种标准协议：

| 客户端 | Base URL | 协议 |
|---|---|---|
| OpenAI 兼容客户端 | `http://host:18090/v1` | Chat Completions |
| Codex CLI | `http://host:18090/v1` | Responses API |
| Claude Code | `http://host:18090` | Anthropic Messages |

同时提供一个单文件 Web 看板（`dashboard.html`，565 KB），覆盖账号、用量、签到、
模型库、智能体配置、设置等全部管理面。

**零第三方依赖**是硬约束：只用 Python 标准库，AES / GCM / RSA / DPAPI / QMC 全部
纯 Python 实现，因此 alpine 容器里也能直接跑。

## 2. 模块地图

```
                       ┌──────────────────────────────────────┐
  HTTP 层              │ qoder_proxy.py  (14974 行)           │
  (ThreadingHTTPServer)│  Handler.do_GET / do_POST            │
                       │  ├─ 鉴权 (_key_ok / _panel_ok)        │
                       │  ├─ 推理入口 (chat/responses/messages)│
                       │  ├─ 管理入口 (accounts/usage/tasks…) │
                       │  └─ 看板静态文件                      │
                       └───────┬──────────────────────────────┘
                               │
     ┌─────────────────────────┼─────────────────────────────┐
     │                         │                             │
┌────▼────────┐        ┌───────▼────────┐          ┌─────────▼────────┐
│ 请求构造     │        │ 账号与调度      │          │ 运营面            │
├─────────────┤        ├────────────────┤          ├──────────────────┤
│ baseprompt  │        │ AccountPool    │          │ qoder_tasks      │
│ flatten     │        │ Account        │          │ qoder_scheduler  │
│ qoder_images│        │ OAuth/PAT      │          │ qoder_settings   │
│ 协议转换     │        │ credits/quota  │          │ usage 记账       │
│ Anthropic   │        │ 冷却/熔断/护栏  │          │ qoder_agents     │
│ Responses   │        │ 机器身份(UMID)  │          │ qoder_webtools   │
└────┬────────┘        └───────┬────────┘          │ qoder_updater    │
     │                         │                    └──────────────────┘
     │                  ┌──────▼─────────┐
     │                  │ 签名与编码      │
     │                  ├────────────────┤
     └─────────────────▶│ qoder_sign     │  COSY 签名 / qoder_encode
                        │ qoder_fingerprint│ derive_id 派生
                        └────────────────┘
```

### 各模块一句话职责

| 模块 | 职责 |
|---|---|
| `qoder_proxy.py` | HTTP 服务、三协议入口、上游请求、SSE 解包、工具回读、用量、看板鉴权 |
| `qoder_accounts.py` | `Account`（单账号状态机）+ `AccountPool`（轮询/亲和/冷却）、OAuth、PAT、额度、机器身份、VPC |
| `qoder_sign.py` | COSY 签名、Qoder 自定义 Base64、纯 Python AES/RSA/GCM/DPAPI/QMC |
| `qoder_fingerprint.py` | `derive_id(uid, salt)` 稳定派生设备身份；本机虚拟化检测 |
| `qoder_catalog.py` | 双区模型清单（动态接口 / 磁盘缓存 / 本机客户端缓存 / 内置快照），别名与显示名解析 |
| `qoder_anthropic.py` | Anthropic Messages ↔ Chat 双向转换（纯翻译，不做形态决策） |
| `qoder_tasks.py` | 签到、活动领取、Pro 福利包、批量执行 |
| `qoder_scheduler.py` | 整点调度器（可配置时刻 + cron 表达式） |
| `qoder_settings.py` | 面板口令（PBKDF2）、API Key 列表、模型闸门、限额护栏、面板会话签名 |
| `qoder_routing.py` | 本地路由规则（模型通配 + 账号组 + Key 绑定 + 分层权重） |
| `qoder_agents.py` | 31 个客户端探测；13 类支持自动配置与还原；也可独立 CLI |
| `qoder_webtools.py` | 网关本地执行 `web_search` / `web_fetch`（SSRF 防护） |
| `qoder_session_cache.py` | 前缀会话亲和（内存 / Redis RESP2 客户端） |
| `qoder_keepalive.py` | 缓存保温：闲置前用同前缀重发 `max_tokens=1` |
| `qoder_pricing.py` | USD/1M tokens 参考价目表（不参与结算） |
| `qoder_images.py` | 内联图片归一化（需 Pillow，缺则原样透传） |
| `qoder_activity.py` | 签到/领取动作流水 `usage/activity.jsonl`（30 天保留） |
| `qoder_updater.py` | GitHub release 检查与一键更新 |
| `qoder_logpaths.py` | 日志文件路径与旧用法目录迁移 |
| `qoder_cache.py` | 有界 TTL 结果缓存 |

## 3. 进程模型

- **单进程多线程**：`ThreadingHTTPServer` + 每个请求一个线程。SSE 流式靠线程阻塞
  读上游 + 写客户端。
- `protocol_version = "HTTP/1.1"`，SSE 用 **chunked 编码**而不是 `Connection: close`。
  原因（代码注释里写得很清楚）：连接池型客户端会把 close 的连接当成可复用，
  下一次请求落在半关闭连接上，表现为"一直重连"。
- `_ExclusiveHTTPServer` 关掉了 `SO_REUSEADDR`：Windows 上默认开启会让两个进程
  绑同一端口、连接被静默分流，极难诊断。
- 后台常驻线程：
  - `qd-models-warm`：启动时预热双区模型清单
  - `CreditsRefresher` / `CreditsUsageRefresher`：余额刷新
  - `Scheduler._thread`：整点巡检
  - `qoder_keepalive._loop`：缓存保温（默认关）
- 生命周期：`SIGTERM` / Windows 控制台关闭都走 `shutdown()`，`atexit` 清理 PID 文件。

## 4. 启动流程（`main()`）

```
解析参数 --host/--port/--lan/--api-key/--accounts-dir/--usage-dir/--panel-password/--pid-file
  ↓
校验 QD_CN_VPC_ENDPOINT（非法直接退出）
  ↓
安装日志分流、迁移旧 usage 目录
  ↓
写 PID 文件（stop 脚本依赖）
  ↓
--lan 时强制要求 API Key（没有就生成并落盘）
面板保存的 Key 优先于自动生成的 LAN Key；命令行优先级最高
  ↓
探测端口是否已被占用（区分 healthy / foreign / stale 三类，issue #89）
  ↓
AccountPool.load() + 读取持久化全局出口
把限额护栏阈值与免费模型名单同批推给池
  ↓
启动余额刷新器 / 调度器 / 模型预热 / 可选保温线程
  ↓
没有账号时只报告本机探测结果，不静默导入（必须用户在看板确认）
  ↓
监听（bind 失败重试 3 次，仍在释放端口时等待）
```

**端口占用三分类**（`classify_existing_instance`）很值得移植：

| 分类 | 判据 | 处理 |
|---|---|---|
| `healthy` | `/health` 应答且应答者 PID 还活着 | 打印"已在运行"并退出 0 |
| `stale` | 有人在应答但那个 PID 已退出（升级瞬间） | 等待重试，不误判双开 |
| `foreign` | 应答者不是本服务 | 报错并给出 `netstat`/`lsof` 提示 |

## 5. 配置体系（三层）

1. **命令行参数**：只影响本次进程（`--host/--port/--lan/--api-key/--accounts-dir/--usage-dir/--panel-password/--system-prompt/--pid-file`）。
2. **`accounts/settings.json`**：面板可改、重启保留。包含
   `panel_password_*`、`api_keys[]`、`launcher_key`、`banned_models`、`limits`、
   `model_context`、`tool_echo_mode`、`anthropic_thinking`、`web_tools`、
   `auth_disabled`、`panel_session_secret`。
3. **环境变量**（`QD_*`）：运维级开关，见 `configuration.md` 全表。

优先级规则在代码里是显式的：

- API Key：命令行 `--api-key` > `settings.json` 里面板保存的 > 自动生成的 `launcher_key`
- 面板口令：`--panel-password` 启动时直接写入 settings
- 全局出口：`QD_PROXY_DEFAULT_REALM` 只作首次默认，之后以看板保存的为准

### settings.json 写入安全

`_settings_is_rewritable()` 是个容易被忽略的关键防御：`load()` 把"文件不存在"和
"解析失败"都返回 `{}`，而 `save()` 写整个 dict。拿读失败的 `{}` 去 save 会抹掉
面板口令哈希、API Key 和限额。所以写之前先确认"文件不存在、或现有内容能解析"，
否则拒绝回写（`panel_session_secret` 会抛异常，调用方退回进程内临时密钥）。

## 6. 鉴权模型

| 标记 | 含义 |
|---|---|
| 公开 | 无需凭证 |
| Key | API Key（`Authorization: Bearer` / `x-api-key` / `?key=`）。**没配置任何 Key 时也可访问** |
| 面板 | `POST /panel/login` 拿 token，之后 `X-Panel-Token` |

关键设计：

- **面板会话同时解锁管理 API**：浏览器无需在 localStorage 存 API Key。
- 面板 token 是**自校验签名**而不查会话表：`<过期秒>.<nonce>.<hmac_sha256>`，
  签名密钥持久化在 `settings.json`，所以**网关重启后浏览器不需要重新登录**。
  改密码时 `revoke_all()` 换密钥，一次性踢掉所有会话。
- 面板还在用默认口令 `admin` 时，`GET /settings/reveal` 返回 403（不泄露明文 Key）。

## 7. 请求状态机（一次 chat 请求）

```
POST /v1/chat/completions
  ├─ _key_ok()                     鉴权；记录用了哪个 key（决定绑定出口、用量归属）
  ├─ 读 body（上限 50 MB，支持 chunked）
  ├─ normalize_tool_choice / normalize_tools / translate_max_completion_tokens
  ├─ _model_precheck(model, realm)  跨区错配 → 全局封禁 → Key 白名单（三入口共用）
  ├─ extract_session_key()          客户端会话 ID（X-Conversation-Id 等）
  ├─ prompt_fingerprint()           隐私安全的提示词指纹（msgs_sha / n_msgs）
  ├─ open_upstream()
  │    ├─ qoder_routing.resolve()   规则解析（realm / allowed_uids / policy）
  │    ├─ detect_model_realm()      独占表 → 清单驱动 → 全局开关
  │    ├─ fallback_realm_by_accounts()  当前区无可用账号则换区
  │    ├─ resolve_model_key()       显示名/别名 → 上游 key
  │    ├─ build_qoder_body()        模板 + 压平 + 参数 + 工具
  │    ├─ [循环] 选号 → assign_prefix_session → qoder_encode → COSY 签名 → POST
  │    │    瞬态故障同账号重试 2 次；排队按 retryAfterSeconds 等待
  │    └─ 全部失败 → RateLimited / 最后一个错误
  ├─ 流式：sse_with_heartbeat(recover_leaked_tool_calls(iter_inner_sse(...)))
  └─ 非流式：aggregate_stream(...) → chat.completion
```

## 8. 值得记住的设计决策

| 决策 | 原因 |
|---|---|
| baseprompt.json 作骨架 | 与官方桌面端请求体逐字段对齐，任何缺字段都可能被风控 |
| 动态模型清单 > 磁盘缓存 > 本机客户端缓存 > 内置快照 | 官方随时改显示名/价格，降级要显式且可观测 |
| 429 起 10 分钟指数退避、最长 2 小时 | 上游限流是账号级，短重试只会加深惩罚 |
| 熔断延迟落账（`blame` 列表） | 全池同样失败（断网/上游整体故障）不能把整池打进 30 分钟熔断 |
| 排队不受账号惩罚 | 10605 是模型级排队，换号不会变快，惩罚唯一账号等于打死它 |
| 出站体积前置拦截 | 阿里云 SLB 对 30MB body 直接 RST，Broken pipe 会被误当瞬态故障反复重试 |
| 单文件 dashboard.html | 无构建、无 CDN、离线可用、便于容器分发 |
