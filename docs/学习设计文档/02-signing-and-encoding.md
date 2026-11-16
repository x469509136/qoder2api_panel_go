# 02 · 签名、编码与设备指纹

这是整个项目最"硬"的部分：所有字节都必须与官方客户端一致，否则上游直接 401 或
拒绝解密。

## 1. Qoder 自定义 Base64（`qoder_encode`）

请求体在发出前会经过一次**变体 Base64**，官方 Go 实现的 `encoding.go` 同构。

```python
CUSTOM = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"
STD    = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
PAD    = "$"     # 标准 '=' 换成 '$'

def qoder_encode(plain):
    std = b64encode(plain).decode()      # 标准 Base64
    n = len(std); a = n // 3
    rearranged = std[n-a:] + std[a:n-a] + std[:a]   # 尾/中/首 三段轮转
    return rearranged.translate(STD2CUSTOM)
```

三步：

1. 标准 Base64（带 `=` 填充）
2. 把字符串切成 **尾 1/3 → 中 1/3 → 首 1/3** 重新拼接
3. 逐字符按自定义字母表映射，`=` → `$`

逆运算（`qoder_decode`，测试用）：先反向映射，再按 `原串 = R3 + R2 + R1` 还原
（注意三段长度：`a = n//3`，首段 `a`、中段 `n-2a`、尾段 `a`）。

**为什么重要**：编码后长度与 Base64 相同（`4*ceil(n/3)`），所以出站体积可以在
编码前用公式估算，不必真的分配几十 MB 的编码字符串——`open_upstream` 就是这么
做的（`_size = 4 * ((len(_plain) + 2) // 3)`）。

## 2. AES-128-CBC（key == iv，PKCS#7）

用于加密 COSY 的 `info` 字段（含 access token / refresh token 的身份 JSON）。

Python 实现要点（Go 用标准库 crypto/aes 即可，这里列出以便对拍）：

- S-box 在导入期由 GF(2^8) 求逆 + 仿射变换**自校验生成**，断言 `SBOX[0x53]==0xED`。
- 密钥扩展同时支持 16 字节（AES-128，10 轮）和 32 字节（AES-256，14 轮）。
- 状态按**列优先** `s[r + 4c]` 排列。
- 加密轮：`SubBytes+ShiftRows → MixColumns → AddRoundKey`。
- 解密轮（用于本地凭证文件）：`InvSubShift → AddRoundKey → InvMixColumns`。

`aes_cbc_decrypt` 做**严格 PKCS#7 校验**（填充字节必须 1..16 且全部相等），
防止把错误密钥解出来的垃圾当明文。

## 3. RSA PKCS#1 v1.5 包裹会话密钥

服务端公钥硬编码（与官方客户端一致）：

```
-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
...
-----END PUBLIC KEY-----
```

实测 `n.bit_length() == 1024`，`e == 65537`。加密流程：

```
k = 128 字节（1024 bit）
PS = 非零随机字节，长度 k-3-len(msg)
EM = 00 02 || PS || 00 || msg
c  = pow(EM_as_int, e, n)
```

`_parse_rsa_pub` 手写 DER TLV 解析（SPKI → BIT STRING → RSAPublicKey → n,e），
Go 里直接用 `x509.ParsePKIXPublicKey` + `rsa.EncryptPKCS1v15` 即可。

> 代码里明确标注了两条已知强度问题并**拒绝单方面加固**：tempKey 只有 64 bit 熵、
> RSA 只有 1024 位。理由是"必须与官方协议逐字节对齐"，改了服务端就解不开。

## 4. COSY 签名（核心）

### 4.1 会话建立（每个账号一份，token 轮换后重建）

```
tempKey  = uuid4().hex[:16]                    # 16 个 ASCII 字符，充当 AES-128 key/iv
cosyKey  = base64(RSA_PKCS1v15(tempKey))       # 上行给服务端解包
identity = {                                   # 9 个键，键名排序后紧凑 JSON
    "name","aid","uid","yx_uid",
    "organization_id","organization_name",
    "user_type","security_oauth_token","refresh_token"
}
info     = base64(AES-128-CBC(json_sorted_compact(identity), tempKey, tempKey))
```

`json_sorted_compact` 的特殊之处：**键排序 + 无空白 + 非 ASCII 不转义**，
且 `None` 序列化成 `""`。服务端签名字节与此强绑定。

### 4.2 每次请求的 Bearer

```
path    = url.path，去掉 "/algo" 前缀
payload = {"cosyVersion","ideVersion":"","info","requestId":uuid4,"version":"v1"}
payload_b64 = base64(json_sorted_compact(payload))
date    = unix 秒（字符串）
sig     = md5(payload_b64 + "\n" + cosyKey + "\n" + date + "\n" + body + "\n" + path)
Bearer  = "COSY." + payload_b64 + "." + sig
```

注意签名覆盖 **body（编码后的字符串）与 path（去 /algo）**，所以：

- 换推理主机（api3 → api2 → api1）**不影响签名有效性**，因为签名只覆盖 path。
- body 一旦重编码就必须重新签。

### 4.3 完整请求头（对齐 qodercli 1.1.75）

```
cosy-data-policy: agree            content-type: application/json
cosy-business-product: cli         cosy-business-type: agent
cosy-scene: assistant              cosy-clienttype: 5
cosy-date: <date>                  cosy-user: <uid>
cosy-key: <cosyKey>                accept: text/event-stream
authorization: Bearer COSY.<payload>.<sig>
accept-encoding: identity          cosy-version: 1.1.75
cosy-machineid: <derive_id(uid,"machine")>
cosy-machinetoken: <derive_machine_token(uid)>
cosy-machinetype: <derive_machine_type(uid)>
cosy-clientip: 169.254.198.161     login-version: v2
user-agent: undici
cosy-machineos: x86_64_win32       （SSE 时另加）
cosy-machinehostname: DESKTOP-XXXXXXX
cache-control: no-cache            （SSE 时加）
x-model-key: <上游 model key>       （推理时加）
x-model-source: system
```

几个细节都有出处：

- `user-agent: undici` —— 官方 CLI 不设 UA，由 Node 的 undici 补默认值。
- `cosy-clientip` 固定 `169.254.198.161`（链路本地地址，官方客户端同款）。
- `accept-encoding: identity` —— 明确要求不压缩，避免 JSON 需要先解压。
- `cosy-machinehostname` 按账号稳定派生 `DESKTOP-` + md5("hostname:"+uid)[:7].upper()。

### 4.4 常量与可覆盖项

| 常量 | 默认 | 覆盖 |
|---|---|---|
| `COSY_VERSION` | `1.1.75` | `QD_COSY_VERSION` |
| `COSY_USER_AGENT` | `undici` | `QD_COSY_UA` |
| `COSY_CLIENT_TYPE` | `5` | `QD_COSY_CLIENTTYPE` |
| `COSY_MACHINE_OS` | Win `x86_64_win32` / 其他 `x86_64_linux` | `QD_COSY_MACHINE_OS`（`auto` 按平台识别） |
| OS 请求头开关 | 开 | `QD_COSY_OS_HEADERS=0` |

`clienttype` 可配的原因（issue #113）：上游按客户端身份决定模型授权，
`kmodel_latest` 对桌面端（`vsc: 999.999.999`）未放量，CLI 才放行。

## 5. 设备指纹（`qoder_fingerprint.py`）

### 5.1 `derive_id(uid, salt)`

```
h = md5(f"{salt}:{uid or 'anonymous'}").hexdigest()      # 32 hex
# 伪造 UUIDv4 形态
h = h[:12] + "4" + h[13:16] + "a" + h[17:]
return f"{h[0:8]}-{h[8:12]}-{h[12:16]}-{h[16:20]}-{h[20:32]}"
```

- 幂等：同一账号永远相同 → 不会因随机机器码触发风控。
- **按账号派生而不是按进程**：网关一个进程冒充几十台设备，如果进程级生成一次，
  所有账号共用一个值，反而把它们关联到同一台机器。
- `QD_DEVICE_ID_LEGACY=1` 保留旧的 32 位无连字符 hex（存量号池升级不"换设备"）。

### 5.2 派生族

| 函数 | 用途 |
|---|---|
| `derive_id(uid,"machine")` | `cosy-machineid` |
| `derive_id(uid,"session")` | 行为事件 sessionId |
| `derive_machine_type(uid)` | `derive_id(uid,"machinetype")` 去横线取前 18 位 |
| `derive_machine_token(uid)` | `base64url(sha512("machinetoken:"+uid))[:43]` |
| `generate_request_id(uid)` | 每请求纯随机 32 hex（**不掺 uid**，否则成为可聚类特征） |

### 5.3 两套身份

| 用途 | 来源 | 特点 |
|---|---|---|
| 推理请求 | `derive_id` 派生 | 账号级，同账号恒定 |
| 签到 / 活动 | 官方 UMID 组件 `runtime-info` 的真实机器身份 | 机器级，同机所有账号共用一个 |

签到链路**只有拿到真实身份才发** `Cosy-Machine*` 头。实测发送派生的假身份反而
会让"每日领取"活动被整条过滤掉。身份缓存到 `accounts/machine_identity.json`，
首次采集多次取样取多数（`QD_MACHINE_IDENTITY_VOTE`）。

### 5.4 本机虚拟化检测（`vm_status`）

两层证据：

1. 官方风控桥 `runtime-info.exe` 返回的 `vmInfo`（最权威：官方客户端就是这么判的）
2. 本机交叉校验：
   - 树莓派等单板计算机（读 `/proc/device-tree/model`）——命中直接结束，不再猜
   - CPU 型号字符串（Windows 注册表 `ProcessorNameString` / Linux `/proc/cpuinfo`）
   - 系统制造商/型号（`HARDWARE\DESCRIPTION\System\BIOS`）
   - 常见虚拟化驱动文件（`vmbus.sys`/`vmci.sys`/`vboxguest.sys`/`virtio_balloon.sys`…）

输出 `{is_vm, level(高/中/低/无/未知), score, brand, brand_cn, source, evidence[], summary}`。
`source` 只能是 `runtime-info` 或 `local`，并且要在 `summary` 里说清"桥不可用"和
"桥可用但没给 vmInfo"是两回事——早期版本把后者谎报成前者，导致看板出现自相矛盾。

## 6. 其他密码学（本地凭证读取用）

| 函数 | 用途 |
|---|---|
| `aes_gcm_decrypt` | 桌面端 `auth.v1.dat` / Chromium os_crypt v10 |
| `chromium_decrypt_v10` | `v10` + nonce12 + AES-256-GCM |
| `dpapi_unprotect` | Windows CryptUnprotectData（ctypes 调用 crypt32） |
| `qmc_decrypt` | 官方模型目录缓存 `catalog-v6`（HKDF-SHA256 + AES-256-GCM，envelope `QMC\x01`+nonce+ciphertext+tag） |

`qmc_decrypt` 的密钥推导：

```
key = HKDF-SHA256(ikm=uid, salt="qoder-model-cache-enc", info="model-cache-v1", L=32)
```

值得注意：它手写了 CTR 模式和 GHASH 校验，并且 **tag 校验失败会明确报
"model cache authentication failed (bad uid?)"** —— 用来区分"uid 不对"和
"文件损坏"。

## 7. Go 移植要点

| Python | Go |
|---|---|
| 手写 AES | `crypto/aes` + `cipher.NewCBCEncrypter` |
| 手写 RSA PKCS#1 v1.5 | `crypto/rsa.EncryptPKCS1v15`（1024 位密钥在 Go 1.24+ 会被拒绝，需 `GODEBUG=rsa1024min=0` 或走 `big.Int` 直接模幂） |
| 手写 GCM | `cipher.NewGCM` |
| 手写 HKDF | `golang.org/x/crypto/hkdf`（标准库 `crypto/hkdf` 在 Go 1.24+ 可用） |
| `ctypes` DPAPI | `syscall.NewLazyDLL("crypt32.dll")` |
| DER 手写解析 | `x509.ParsePKIXPublicKey` |

> **1024 位 RSA + PKCS#1 v1.5 在 Go 里会被 `crypto/rsa` 拒绝**（Go 1.24 起要求
> ≥1024 且对 v1.5 有额外限制）。本项目 Go 实现直接走 `math/big` 模幂，
> 行为与 Python 的 `pow(m, e, n)` 逐字节一致，也避免受标准库策略变更影响。
