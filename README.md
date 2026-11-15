# bridge_qoder

Qoder（qoder.com.cn / qoder.com）→ OpenAI / Anthropic 兼容网关，**Go 实现**。

Python 原版（`qoder2api-hub`，约 3.8 万行）的逐模块精读见 [`docs/`](docs/README.md)，
移植方案见 [`docs/10-go-port-plan.md`](docs/10-go-port-plan.md)。

## 特性

- **三种协议**：OpenAI Chat Completions、Responses API（Codex）、Anthropic Messages（Claude Code）
- **双区多账号**：国内版 / 国际版独立账号池与出口，按模型自动选区；轮询、会话亲和、冷却换号
- **免客户端登录**：OAuth 设备授权（PKCE S256）、PAT 导入、本机凭证扫描
- **模型与官方同源**：动态接口 > 磁盘缓存 > 本机客户端缓存 > 内置快照，四级降级
- **签到与保活**：每日领取、活动平台、Token 定时刷新
- **防风控**：每账号一套派生设备身份；COSY 签名与官方客户端逐字节对齐
- **零第三方依赖**：只用 Go 标准库（含手写 RESP2 Redis 客户端）
- **Web 看板**：单文件 `dashboard.html`，原样复用

## 快速开始

```bash
go build -o qoder-proxy ./cmd/qoder-proxy
./qoder-proxy --host 127.0.0.1 --port 8790
```

首次启动后打开 `http://127.0.0.1:8790/`，默认面板密码 `admin`（请立刻修改），
在「账号」页添加账号。

`assets/`、`web/`、`i18n/` 需与可执行文件同级（模型快照与 baseprompt 已内嵌，
缺失外部文件时自动回退）。

## 接入客户端

| 客户端 | Base URL | 协议 |
|---|---|---|
| OpenAI 兼容客户端 | `http://127.0.0.1:8790/v1` | Chat Completions |
| Codex CLI | `http://127.0.0.1:8790/v1` | Responses API |
| Claude Code | `http://127.0.0.1:8790` | Anthropic Messages |

```bash
curl http://127.0.0.1:8790/v1/chat/completions \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model": "Qwen3.8-Flash", "messages": [{"role": "user", "content": "你好"}]}'
```

## 开发

```bash
go build ./...        # 构建
go vet ./...          # 静态检查
go test ./...         # 单元 + 端到端测试
go test -race ./...   # 竞态检测
```

端到端测试会起一个假的 Qoder 上游，验证签名、编码、信封解包、守卫链与三协议转换，
不需要真实账号。

## 目录

```
cmd/qoder-proxy/     进程入口
internal/            网关实现（见 docs/10-go-port-plan.md）
web/                 单文件看板
i18n/                语言包
assets/              baseprompt.json + 双区模型快照
docs/                学习文档与移植说明
```

## 免责声明

本项目是非官方的自托管网关，仅供技术研究，以及个人在私有环境中使用自己合法授权的
账号。请遵守 Qoder 官方服务条款，不得用于商业转售、恶意并发等滥用行为。
