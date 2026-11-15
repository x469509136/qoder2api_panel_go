# Qoder2API-Hub 学习文档

本目录是对 `D:\github\bridge\qoder2api-hub`（Python 实现，38k 行）的逐模块精读笔记，
并给出将其**全部功能用 Go 重写**的设计与实现（代码在 `../` 下的 Go module）。

阅读顺序建议：先看 01 建立全局图景，再看 02–03 理解"请求怎么变成合法上游调用"，
04–06 是账号与协议的细节，07–09 是运营面（调度、用量、管理接口），10 是 Go 移植方案。

| 文档 | 内容 |
|---|---|
| [01-architecture.md](01-architecture.md) | 项目定位、模块地图、进程模型、启动流程、配置体系 |
| [02-signing-and-encoding.md](02-signing-and-encoding.md) | 自定义 Base64、AES-CBC、RSA PKCS#1 v1.5、COSY 签名、设备指纹 |
| [03-request-pipeline.md](03-request-pipeline.md) | baseprompt 模板、会话压平、图片预处理、body 构造、SSE 信封解包 |
| [04-accounts-and-realms.md](04-accounts-and-realms.md) | 双区常量、账号对象、OAuth 设备授权、PAT、冷却/熔断/护栏、签到与活动 |
| [05-protocols.md](05-protocols.md) | OpenAI Chat、Responses API、Anthropic Messages 三协议双向转换 |
| [06-tool-call-guards.md](06-tool-call-guards.md) | 工具调用泄漏回读、DSML、伪 XML、参数修复、fail-closed/lenient |
| [07-routing-and-scheduling.md](07-routing-and-scheduling.md) | 双区路由、会话亲和、前缀缓存保温、调度器与定时任务 |
| [08-usage-and-observability.md](08-usage-and-observability.md) | 用量记账、积分折算、性能指标、洞察接口、日志 |
| [09-admin-api.md](09-admin-api.md) | 全量 HTTP 端点清单与鉴权矩阵 |
| [10-go-port-plan.md](10-go-port-plan.md) | Go 重写的包结构、映射表、差异与取舍、构建运行 |
| [11-go-implementation-notes.md](11-go-implementation-notes.md) | Go 实现要点：关键算法落点、并发模型、测试策略 |

## 一句话总结

它是一个**协议翻译 + 多账号调度 + 上游身份伪装**的网关：

```
客户端（OpenAI / Responses / Anthropic）
   → 归一成 Chat 请求
   → 套官方 baseprompt 模板、压平会话、清洗工具历史
   → Qoder 自定义 Base64 编码
   → COSY 签名（RSA 包 AES 会话密钥 + MD5 请求签名）
   → POST gateway/algo/api/v2/service/pro/sse/agent_chat_generation
   → 解开 {"headers","body","statusCodeValue"} SSE 信封
   → 回读泄漏的工具调用、清洗空字段
   → 按客户端协议重新编码回 SSE / JSON
```

三条贯穿全项目的主线，读代码时抓住它们就不会迷路：

1. **前缀字节稳定性**：上游按账号缓存 prompt 前缀，任何一轮之间字节变化都会让缓存
   命中率归零。所以会话压平、工具结果截短、Anthropic 的 messages 内 system 处理，
   全部围绕"同样的历史永远序列化成同样的字节"设计。
2. **单账号一台设备**：每账号的 machineId/hostname/token 都由 UID 加盐派生，
   跨账号互不相同，避免被上游关联成"同一台机器刷几十个号"。
3. **fail-closed 而非静默成功**：上游把工具调用吐成正文、参数被截断时，宁可报错
   也不把内部标记透给用户，也不假装正常结束。

## 原始文档索引（源仓库 docs/）

- `internals.md`：请求链路、设备指纹、容错表、流式、工具调用、前缀缓存
- `configuration.md`：全部命令行参数与环境变量（约 90 个）
- `accounts.md`：账号、凭证生命周期、签到规则、VPC、团队账号
- `models.md`：模型来源四级降级、命名、双区路由、峰谷价、思考档位
- `api.md`：HTTP 端点、鉴权、额度构成、探活
- `risk-control-and-queueing.md` / `risk-control-playbook.md`：风控维度与处置
- `clients.md` / `deployment.md` / `troubleshooting.md`：接入与运维

## 代码规模（Python 原文）

| 文件 | 行数 | 职责 |
|---|---:|---|
| `qoder_proxy.py` | 14974 | 主程序：路由、上游、协议转换、用量、看板鉴权 |
| `qoder_accounts.py` | 5465 | 双区账号池、OAuth、PAT、额度、机器身份 |
| `qoder_catalog.py` | 2186 | 官方模型清单加载与别名解析 |
| `qoder_agents.py` | 1244 | 31 个客户端的探测与一键配置 |
| `qoder_tasks.py` | 1055 | 签到、活动、福利包 |
| `qoder_anthropic.py` | 802 | Anthropic ↔ Chat 转换 |
| `qoder_settings.py` | 737 | 面板口令、API Key、模型闸门、限额、面板会话 |
| `qoder_sign.py` | 718 | COSY 签名与纯 Python 密码学 |
| `qoder_scheduler.py` | 530 | 整点调度 |
| `qoder_routing.py` | 359 | 本地路由规则 |
| `qoder_webtools.py` | 477 | 本地 web_search / web_fetch |
| `qoder_fingerprint.py` | 336 | 设备指纹派生与虚拟机检测 |
| 其余 | ~1500 | 缓存、图片、定价、保温、更新、活动流水、日志路径 |
| `dashboard.html` | 565KB | 单文件看板 |

测试：`tests/` 下 150 个套件（约 1.3 万行），`_test_qoder.py` 是旧单文件入口。
