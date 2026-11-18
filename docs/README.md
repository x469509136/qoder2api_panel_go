# bridge_qoder

Qoder（qoder.com.cn / qoder.com）→ OpenAI / Anthropic 兼容网关，**Go 实现**。

[![License: MIT](https://img.shields.io/badge/License-MIT-green?style=flat-square)](LICENSE)

目录说明：

- 本文件 = 项目入口（特性 / 快速开始 / 接入方式）。
- 交接文档（需求、架构、变更、踩坑、故障、模型）与本文同目录，见下方索引。
- Python 原版（`qoder2api-hub`，约 3.8 万行）逐模块精读与 Go 移植方案在
  [`学习设计文档/`](学习设计文档/README.md)。

## 交接先看这六篇

| 文档 | 什么情况下看 |
|---|---|
| [需求文档.md](需求文档.md) | 想知道做过什么、验收标准是什么 |
| [架构说明.md](架构说明.md) | 想了解代码分层、包职责、数据流、目录结构 |
| [变更记录.md](变更记录.md) | 想查某次改动的范围与理由 |
| [踩坑笔记.md](踩坑笔记.md) | **动手改代码前必读**，避免重犯已知错误 |
| [故障排查.md](故障排查.md) | 线上出问题时的排查路径 |
| [模型交接文档.md](模型交接文档.md) | 模型清单、Codex 接入布局、实测数据 |

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

源码在 `code/`，先切进去：

```bash
cd code
go build -o qoder-proxy ./cmd/qoder-proxy
./qoder-proxy --host 127.0.0.1 --port 18090
```

首次启动后打开 `http://127.0.0.1:18090/`，默认面板密码 `admin`（请立刻修改），
在「账号」页添加账号。

端口优先级 `--port` > `PORT` 环境变量 > 默认 `18090`，越界（>65535）会直接启动失败。

`assets/`、`web/`、`i18n/` 需与可执行文件同级（模型快照与 baseprompt 已内嵌，
缺失外部文件时自动回退）。运行时生成 `accounts/`、`usage/`、`logs/`，三者均已忽略。

Windows 下也可以直接用构建脚本（产物落 `dist-win/`，已忽略）：

```bat
cmd_bat_sh\win10_cmd\build-win10.bat          REM release
cmd_bat_sh\win10_cmd\build-win10.bat debug    REM 带符号
cmd_bat_sh\win10_cmd\build-win10.bat clean    REM 清理产物目录
```

脚本会先跑 `go vet ./...`；因此**仓库编译不过时它必然失败**。脚本同目录缺 `start-win10.bat` /
`stop-win10.bat`（见《变更记录》本轮小节，属待补配套，不影响 release / debug / clean 的编译主流程）。

## 接入客户端

| 客户端 | Base URL | 协议 |
|---|---|---|
| OpenAI 兼容客户端 | `http://127.0.0.1:18090/v1` | Chat Completions |
| Codex CLI | `http://127.0.0.1:18090/v1` | Responses API |
| Claude Code | `http://127.0.0.1:18090` | Anthropic Messages |

```bash
curl http://127.0.0.1:18090/v1/chat/completions \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model": "Qwen3.8-Flash", "messages": [{"role": "user", "content": "你好"}]}'
```

## 开发

```bash
cd code
go build ./...        # 构建
go vet ./...          # 静态检查
go test ./...         # 单元 + 端到端测试
go test -race ./...   # 竞态检测
```

端到端测试会起一个假的 Qoder 上游，验证签名、编码、信封解包、守卫链与三协议转换，
不需要真实账号。

## 目录

```
code/cmd/qoder-proxy/     进程入口
code/internal/            网关实现（见 docs/学习设计文档/10-go-port-plan.md）
code/internal/qoder/      Qoder 业务协议（拆包已完成，不依赖账号池）
code/web/                 单文件看板
code/i18n/                语言包
code/assets/              baseprompt.json + 双区模型快照
code/tools/               add_qoder_models.py（导入 Codex 模型目录）
code/accounts/ usage/ logs/   运行时生成，已忽略
cmd_bat_sh/               构建脚本（win10_cmd/build-win10.bat）
dist-win/                 build-win10.bat 的默认产物目录（已忽略）
docs/                     交接文档
docs/学习设计文档/          Python 精读 + Go 移植说明
```

## 免责声明

本项目是非官方的自托管网关，仅供技术研究，以及个人在私有环境中使用自己合法授权的
账号。本项目不提供任何账号或额度。请遵守 Qoder 官方服务条款，不得用于商业转售、
恶意并发等滥用行为。

[MIT License](LICENSE)
