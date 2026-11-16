# 10 · Go 移植方案

本篇说明 Python 版的每一块能力落到 Go 的哪个包，以及移植时**必须保持**和
**可以合理改变**的地方。

## 1. 包结构

以下路径相对 `code/`（2026-11-16 起源码从仓库根搬到 `code/`，`go.mod` 同在该目录）：

```
code/cmd/qoder-proxy/main.go  进程入口：参数、日志、端口探测、信号
code/internal/
  server/     HTTP 面：三协议入口、鉴权、管理 API、上游编排
  protocol/   协议转换：baseprompt 构造、会话压平、Anthropic/Responses、
              SSE 事件翻译、图片预处理
  guards/     工具调用回声回读与守卫链（fail-closed/lenient）
  upstream/   签名请求、SSE 信封解包、错误分类、队列解析、内层错误观测
  sign/       COSY 签名、Qoder 自定义 Base64、AES/RSA/GCM
  fingerprint/设备身份派生（derive_id 族）、VM 检测
  accounts/   账号对象、双区常量、池、OAuth/PAT、额度、签到、扫描
  catalog/    官方模型清单快照、别名解析、档位、峰谷价
  settings/   面板口令、API Key、模型闸门、限额、面板会话
  route/      本地路由规则与分层选号
  session/    前缀会话亲和（内存 / Redis）
  usage/      用量记账与全部统计报表
  tasks/      签到、活动领取、批量执行
  scheduler/  整点调度 + cron + 已触发点持久化
  keepalive/  缓存保温
  webtools/   本地 web_search / web_fetch（SSRF 防护）
  agents/     31 个客户端探测与配置写入
  pricing/    参考价目表
  activity/   签到/领取流水
  logx/       日志三出口 + 环形缓冲
code/web/dashboard.html   单文件看板（原样复用）
code/i18n/en.json         语言包（原样复用）
code/assets/              baseprompt.json + 双区模型快照
```

## 2. 逐模块映射

| Python | Go 包 | 说明 |
|---|---|---|
| `qoder_proxy.py`（14974 行） | `server/` + `protocol/` + `guards/` + `upstream/` | 按职责切成四块 |
| `qoder_sign.py` | `sign/` | 算法换成标准库，输出字节不变 |
| `qoder_fingerprint.py` | `fingerprint/` | 派生逻辑逐行照搬 |
| `qoder_accounts.py` | `accounts/` | 拆成 realm / account / pool / login / quota / checkin / scan / httpclient |
| `qoder_catalog.py` | `catalog/` | 快照改为 `go:embed` |
| `qoder_settings.py` | `settings/` | 加 `Update(func)` 保证读改写原子 |
| `qoder_routing.py` | `route/` | 修掉原实现的锁内重入风险 |
| `qoder_session_cache.py` | `session/` | 手写 RESP2 客户端，仍然零依赖 |
| `qoder_anthropic.py` | `protocol/anthropic*.go` | 只做翻译，不做形态决策 |
| `qoder_tasks.py` | `tasks/` | 并行上游查询保留 |
| `qoder_scheduler.py` | `scheduler/` | 到点即补 + 已触发点落盘 |
| `qoder_keepalive.py` | `keepalive/` | 同机制 |
| `qoder_webtools.py` | `webtools/` | SSRF 边界加强（钉 IP + 逐跳复检） |
| `qoder_agents.py` | `agents/` | 13 类自动配置 + 手动指引 |
| `qoder_pricing.py` / `qoder_activity.py` / `qoder_logpaths.py` / `qoder_cache.py` | `pricing/` `activity/` `logx/` | 合并到对应包 |
| `qoder_images.py` | `protocol/images.go` | 不依赖 Pillow：只重压 PNG 的 IDAT |
| `qoder_updater.py` | `server/admin_misc.go` | **不支持**进程内自更新（见 §6） |
| `dashboard.html` | `web/dashboard.html` | 原样复用（565 KB） |

## 3. 必须逐字节保持的部分

这些地方一旦"优化"就会失败，移植时必须对拍：

| 项 | 校验方式 |
|---|---|
| `qoder_encode` | 已有 golden 测试，输出与 Python 完全一致 |
| `json_sorted_compact` | 键排序 + 无空格 + 非 ASCII 不转义 + `nil` → `""` |
| COSY Bearer | `md5(payload_b64 \n cosyKey \n date \n body \n path)`，path 去 `/algo` |
| 身份 JSON 的 9 个键 | 键名与顺序（排序后）都固定 |
| `derive_id` 形态 | UUIDv4 形状：第 13 位 `4`、第 17 位 `a` |
| baseprompt 骨架 | `messages` 只保留模板 system；示例 user 轮次必须丢弃 |
| 工具历史文本化 | `[assistant 请求调用工具]` + JSON 数组 |
| 工具结果标记 | `[工具结果(name)]\n`，截短后的省略串**不含字数** |

## 4. 允许改变的部分（并在文档里写清）

| 项 | Python | Go | 理由 |
|---|---|---|---|
| AES/RSA/GCM | 纯 Python 手写 | `crypto/aes`、`math/big` 模幂 | 标准库更快更稳；RSA 仍手写模幂以支持 1024 位 |
| 图片归一 | 需要 Pillow，缺则透传 | 只重压 PNG 的 IDAT（无需解码器） | 目标只有"同图同字节"，不降分辨率不留元数据 |
| 并发模型 | 每请求一线程 | net/http 每请求一 goroutine | 天然 |
| 日志 | print + 文件 | 三出口 + 环形缓冲 | 面板 /logs 行为一致 |
| 进程内自更新 | 支持 | 不支持，明确报"用部署流水线" | 自改可执行文件是风险，不是功能 |

## 5. 关键实现细节

### 5.1 RSA 1024 位必须手写模幂

`crypto/rsa.EncryptPKCS1v15` 在新版 Go 里会拒绝 1024 位密钥。服务端公钥是官方
客户端硬编码的、不可替换，所以：

```go
m := new(big.Int).SetBytes(em)
c := new(big.Int).Exp(m, big.NewInt(int64(e)), n)
```

结果与 Python 的 `pow(m, e, n)` 逐字节一致。

### 5.2 前缀缓存要求会话 id 真随机

Windows 的时钟精度约 15 ms，用 `time.Now().UnixNano()` 派生 UUID 会让同一 tick
内的两次调用撞号，进而把两个不同会话映射到同一个上游 session。必须用
`crypto/rand`。这是测试 `TestMemoryPrefixAffinity` 抓出来的真实缺陷。

### 5.3 锁不可重入

Python 用 `threading.RLock`，Go 的 `sync.Mutex` 不可重入。路由配置的 `Save` 里
再调 `Load`（两者都取锁）会直接死锁 —— 测试 `TestSaveRevisionConflict` 抓到过。
修法：`Save` 先 `Load` 再写，不跨调用持锁。

### 5.4 出站体积用公式预估

Qoder 编码保持 Base64 的填充长度，所以：

```go
size := 4 * ((len(plain) + 2) / 3)
```

不必真的分配几十 MB 的编码字符串就能在本地拦下 413。

### 5.5 SSE 必须是 chunked + keep-alive

`net/http` 默认就是这样（不设 `Content-Length` 即 chunked）。不要为了"立刻刷出"
手动加 `Connection: close`：连接池型客户端会把 close 的连接当可复用，下一次请求
落在半关闭 socket 上，表现为"一直重连"。

### 5.6 守卫链是纯函数

`guards.Process(frames, allowed)` 输入输出都是 `[]Frame`，不依赖并发也不依赖
socket。这带来两个好处：

- 单元测试可以直接喂真实泄漏样本；
- 三条链路（chat / Responses / Anthropic）共用同一条链，不会漏。

各阶段的缓冲是**各自独立**的（marker / DSML / tool tag 各一份），避免互相抢字节
—— 这正是 Python 版的结构，照搬即可。

### 5.7 正则不能用前瞻

Go 的 `regexp` 是 RE2，不支持 `(?=...)`。Python 里三处前瞻都改写成"捕获分隔符
再由调用方裁剪"。这是**必须**检查的移植点，否则运行期直接 panic。

## 6. 有意不做的事

| 项 | 理由 |
|---|---|
| 进程内自更新 | 自改可执行文件在生产环境是风险；`/update/*` 只报告状态 |
| UMID 原生机器身份 | 需要打包官方二进制 `runtime-info`；Go 版只发派生的身份，签到链路在没有原生身份时按"不发送机器头"处理（这本身是安全侧） |
| 完整 i18n 提取工具链 | `i18n/en.json` 原样复用，路由与清单已实现 |

## 7. 构建与运行

```bash
cd code
go build -o qoder-proxy ./cmd/qoder-proxy
./qoder-proxy --host 127.0.0.1 --port 18090
```

`assets/`、`web/`、`i18n/` 需要在可执行文件同级目录（或工作目录），即 `code/` 下。
模型快照与 baseprompt 同时内嵌了一份（`go:embed`），缺失外部文件时自动回退。

交叉编译：

```bash
GOOS=linux GOARCH=amd64 go build -o qoder-proxy-linux ./cmd/qoder-proxy
GOOS=linux GOARCH=arm64 go build -o qoder-proxy-arm64 ./cmd/qoder-proxy
```

## 8. 测试

```bash
go test ./...            # 全部
go vet ./...
```

当前覆盖：

| 包 | 内容 |
|---|---|
| `sign` | 编码 golden 对拍、AES/GCM 往返、RSA 边界、签名形态、派生形态 |
| `guards` | marker/DSML/伪 XML 回读、散文逐字节守恒、fail-closed、id 补齐、index 归一 |
| `protocol` | 压平两种形态、截短字节稳定、三协议转换、usage 守恒、档位 clamp |
| `accounts` | 冷却/熔断/降权、护栏 fail-open、402 停放、UTC+8 窗口、持久化、VPC、额度归一 |
| `settings` | 口令摘要、会话跨重启、Key 匹配、闸门、限额继承、拒绝覆盖损坏文件 |
| `route` | 规则匹配、范围**只收窄**、分层与三种策略、revision 冲突 |
| `session` | 前缀亲和、系统提示不合并会话、LRU 上界、Redis URL 解析 |
| `usage` | 记账、积分折算、全价请求、分位数、分组报表 |
| `upstream` | 信封解包、队列解析、错误分类、内层错误归类 |
| `catalog` | 名称解析四形态、动态显示名、独占区、自动选区、档位顺序与 clamp |
| `server` | **端到端**：签名+编码+信封+守卫+三协议+排队重试+413 本地拦截 |
