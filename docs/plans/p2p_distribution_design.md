# P2P 节点互联与自动分发

## 元数据

- 文档类型：Plan
- 版本：0.6.0
- 所属项目：phaethon
- 创建日期：2026-09-08

## 变更记录

| 版本 | 日期 | 变更内容 | 作者 |
|------|------|----------|------|
| 0.1.0 | 2026-09-08 | 初始版本 | Qoder |
| 0.2.0 | 2026-09-08 | 按 mdd 规范重写，复用统一帧协议 | Qoder |
| 0.3.0 | 2026-09-08 | 恢复 PORT=2 独立通道，移除控制连接复用 | Qoder |
| 0.4.0 | 2026-09-09 | 删除 ResolveCmd，各 dialer 显式指定命令 | Qoder |
| 0.5.0 | 2026-09-09 | P2P 模块骨架 + 版本交换 + 二进制传输实现 | Qoder |
| 0.6.0 | 2026-09-09 | 重新设计分发模型（库存式分发）、版本比较、运行时平台检测、自更新与 watchdog 交接机制 | Qoder |

---

## 1. 背景与目标

### 1.1 当前问题

phaethon 部署在多个环境（QG/VM/JF），版本更新全靠手动编译、scp 上传、重启。节点越多越难维护。

### 1.2 目标

1. 两个 phaethon 实例建立代理关系后，**自动**形成 P2P 对等连接
2. 对等连接上交换**库存清单**，自动同步缺失或低版本的二进制
3. 每个节点都是一个分发点 — 二进制像种子一样在网络中扩散
4. 用户无感知 — 不需要额外配置
5. P2P 通道可扩展 — 未来可承载配置同步、日志聚合、健康监控等

---

## 2. 架构设计

### 2.1 BIND PORT 扩展

复用现有 BIND PORT 字段区分连接类型，与 PORT=0/1 完全同构：

| BIND PORT | 含义 | 注册端行为 |
|-----------|------|-----------|
| `0` | 数据连接 | 入 Registry 等待 PONG 匹配 |
| `1` | 控制连接 | 入 ControlManager，处理 register 等命令 |
| `2` | P2P 连接 | 入 P2PManager，交换库存、分发二进制 |

**不新增帧类型。** 所有连接复用统一帧协议（`reverse/frame.go`），P2P 消息通过 `FrameData`(0x05) 承载 JSON 命令，心跳复用 `FrameHeartbeat`(0x01)。

### 2.2 连接建立流程

```
Dialer 端（自动触发）                     Registry 端
  │                                         │
  │ ═══ PORT=2 独立通道 ═══                 │
  │                                         │
  │ SOCKS5/Trojan:                          │
  │   ChainDial → BIND(server, PORT=2)      │
  │                     ↓                    │
  │               dstPort == 2               │
  │                     ↓                    │
  │               handleP2PConn()            │
  │                                         │
  │ h_tunnel:                               │
  │   DialP2P() → 硬编码 BIND PORT=2        │
  │                     ↓                    │
  │               port == 2                  │
  │                     ↓                    │
  │               handleP2PConn()            │
  │                                         │
  │  成功 → 双方交换 hello → 进入 P2P   │
  │  失败 → 静默跳过，不影响正常代理功能      │
```

### 2.3 删除 ResolveCmd，各 dialer 显式指定命令

已实现。`ResolveCmd` 和 `IsBind` 已删除，各方法显式指定命令：

| 方法 | SOCKS5/Trojan | HTunnel |
|------|--------------|---------|
| `Dial()` | CONNECT（默认） | CONN（默认） |
| `DialControl()` | 硬编码 BIND PORT=1 | 硬编码 BIND PORT=1 |
| `DialP2P()` | 硬编码 BIND PORT=2 | 硬编码 BIND PORT=2 |

### 2.4 自动触发机制

```
启动时遍历 proxies 列表：
  if proxy.Type ∈ {socks5, trojan, h_tunnel} && proxy.Server != "" {
      go p2pManager.StartPeer(proxy)
      // 尝试 PORT=2 BIND，失败则指数退避重连（1s→60s）
  }
```

不需要用户配置任何 P2P 相关字段。

---

## 3. P2P 协议

### 3.1 传输层

复用统一反向连接帧协议（`reverse/frame.go`），不新增帧类型：

```
TYPE(1B) + LENGTH(2B, big-endian) + PAYLOAD(0~65535B)
```

| 帧类型 | 用途 |
|--------|------|
| `FrameHeartbeat`(0x01) | 心跳保活，双向，10s 间隔，60s 超时 |
| `FrameData`(0x05) | P2P 命令与数据 |

### 3.2 命令格式

PORT=2 专用于 P2P，无需命令前缀。命令为 JSON，通过 `FrameData`(0x05) 传输：

```json
{"cmd": "xxx", ...字段}
```

### 3.3 命令列表

#### hello（双向，连接建立后各发一个）

```json
{
  "cmd": "hello",
  "nodeId": "phaethon-vm",
  "version": "v1.2.3",
  "platform": "windows",
  "arch": "amd64",
  "inventory": [
    {"platform": "windows", "arch": "amd64", "version": "v1.2.3"},
    {"platform": "linux", "arch": "amd64", "version": "v1.2.0"}
  ]
}
```

- `inventory`：本节点 `p2p-cache/` 目录中持有的所有二进制清单
- 收到 hello 后，对比双方清单，同步自己没有的或版本低的

#### update_request（请求同步二进制）

```json
{
  "cmd": "update_request",
  "platform": "linux",
  "arch": "amd64",
  "version": "v1.2.3"
}
```

- 请求指定 platform/arch/version 的二进制
- 发送方从 `p2p-cache/` 中读取对应文件发送

#### manifest（发送方 → 请求方，二进制分片清单）

```json
{
  "cmd": "manifest",
  "platform": "linux",
  "arch": "amd64",
  "version": "v1.2.3",
  "totalSize": 15728640,
  "fileHash": "sha256:whole_file_hash...",
  "chunkSize": 524288,
  "totalChunks": 30,
  "chunkHashes": [
    "sha256:chunk0_hash...",
    "sha256:chunk1_hash..."
  ]
}
```

#### chunk_req（请求方 → 发送方，请求数据块）

```json
{
  "cmd": "chunk_req",
  "chunkIndex": 0
}
```

#### chunk_data（发送方 → 请求方，返回数据块）

FrameData payload 直接为原始二进制（非 JSON）。

> 帧协议最大 payload 65535 字节，512KB chunk 需分多个帧发送。
> 每个 chunk 的帧序列：先送一帧 JSON 头 `{"chunkIndex":N, "frameSeq":0, "totalFrames":8}`，
> 后续帧为纯二进制 payload。

#### update_ack（请求方 → 发送方）

```json
{
  "cmd": "update_ack",
  "status": "ok",
  "fileHash": "sha256:verified_hash..."
}
```

status 可选值：`ok` / `chunk_mismatch` / `error`

### 3.4 时序图

```
节点 A                                  节点 B
  │                                        │
  │ ←── P2P 连接已建立（PORT=2）──→         │
  │                                        │
  │ ── hello(inventory_A) ────────→        │
  │ ←── hello(inventory_B) ────────        │
  │                                        │
  │  对比清单：                              │
  │  A 发现 B 有 linux/amd64/v1.2.3        │
  │  A 自己没有或版本更低                     │
  │                                        │
  │ ── update_request(linux/amd64/v1.2.3) →│
  │                                        │
  │              B 从 p2p-cache/ 读取文件   │
  │              计算分片哈希                │
  │                                        │
  │ ←── manifest ──────────────────        │
  │                                        │
  │ ── chunk_req(0) ──────────────→        │
  │ ←── chunk_data(0) ────────────         │  校验 chunkHashes[0]
  │                                        │
  │ ── chunk_req(1) ──────────────→        │
  │ ←── chunk_data(1) ────────────         │  校验 chunkHashes[1]
  │ ...                                    │
  │                                        │
  │  全部收完，校验 fileHash                │
  │  存入 p2p-cache/                       │
  │                                        │
  │ ── update_ack(ok) ────────────→        │
  │                                        │
  │  检查：收到的二进制是否匹配自己架构       │
  │  且版本高于当前运行？                    │
  │  → 是：触发自更新流程                    │
  │  → 否：仅存缓存，等待分发                │
```

---

## 4. 库存式分发模型

### 4.1 核心理念

每个节点是一个**分发网络中的对等节点**，持有并分发多个平台/版本的二进制：

- 同步：跟 peer 交换清单，自己没有的或版本低的就拉取
- 存储：拉下来的二进制存入 `p2p-cache/`
- 分发：缓存里有的就能给别人，不管自己是不是在跑
- 替换：同步完后，检查有没有匹配自己架构且版本更高的 → 有就替换运行

### 4.2 缓存目录

```
<workdir>/p2p-cache/
  phaethon_linux_amd64_v1.2.3         ← 自己正在跑的（启动时放入）
  phaethon_windows_amd64_v1.2.3       ← 从 peer 同步来的，帮别人存
  phaethon_darwin_arm64_v1.1.0        ← 从 peer 同步来的，帮别人存
```

文件命名：`phaethon_{platform}_{arch}_{version}`

### 4.3 同步规则

```
1. 启动时，把自己当前二进制放入 p2p-cache/（如果还没有）
2. 跟 peer 交换各自的 p2p-cache/ 清单
3. 对比：对方有我没有的，或版本比我高的 → 拉取存入 p2p-cache/
4. 拉完后扫描：有没有 platform/arch 匹配自己 + 版本高于当前运行的？
   → 有就触发自更新
```

### 4.4 版本比较

版本字符串来自 `git describe --tags --always --dirty`，格式包括：
- `v1.2.3` — 语义化标签
- `v1.2.3-5-gabc1234` — 标签后 5 个 commit，比 `v1.2.3` 新
- `78e9dfc` — 无标签的 commit hash
- `dev` — 开发版本，最低

比较规则：
1. `dev` 是最低版本
2. 有标签的 > 无标签的
3. 标签相同，commit 数多的更新
4. 都无标签，字符串比较

### 4.5 运行时平台检测

不依赖编译参数，运行时自动检测：

- **Windows**：通过 `RtlGetVersion` (ntdll.dll) 检测 Windows 版本
  - majorVersion < 10（Win7/8/8.1）→ buildTag = `"win7"`
  - majorVersion >= 10（Win10+）→ buildTag = `""`
- **非 Windows**：buildTag = `""`

hello 交换时 platform + arch + buildTag 三者都匹配才认为兼容，才进行二进制同步。

---

## 5. 自更新流程

### 5.1 接收与校验

```
1. 收到 manifest → 解析 totalSize、fileHash、chunkHashes
2. 创建临时文件 p2p-cache/.tmp_xxx
3. 逐块请求：chunk_req(N) → 接收 chunk_data(N)
4. 每块接收后立即校验 SHA-256
5. 不匹配 → 删除临时文件，发送 update_ack{status:"chunk_mismatch"}
6. 全部匹配 → 校验 fileHash
7. 校验通过 → rename 临时文件为正式缓存文件
8. 发送 update_ack{status:"ok"}
```

### 5.2 替换决策

收到新二进制后：
- platform/arch/buildTag 匹配自己 **且** 版本高于当前运行 → **触发自更新**
- 否则 → 仅存入缓存，等待分发给其他 peer

### 5.3 Worker 退出码协议

Worker 完成二进制替换后，使用特殊退出码通知 watchdog：

| 退出码 | 含义 |
|--------|------|
| `0` | 正常退出（watchdog 不重启） |
| `42` | 请求更新（watchdog 执行更新交接流程） |
| 其他 | 异常退出（watchdog 重启同版本 worker） |

### 5.4 自更新流程（分平台）

#### Linux — 进程内替换

```
Worker:
  1. 新二进制已校验通过，存入 p2p-cache/
  2. rename(当前二进制 → phaethon.bak)     ← Linux 允许 rename 运行中的文件
  3. copy(p2p-cache/新版本 → 原路径)
  4. chmod +x
  5. execve() 替换自身                      ← 同一 PID，新代码运行
  6. 新进程删除 phaethon.bak

Watchdog:
  完全无感知 — 它监控的 PID 还活着，只是内容换了
```

#### Windows — Watchdog 交接

```
Worker:
  1. 新二进制已校验通过，存入 p2p-cache/
  2. rename(phaethon.exe → phaethon.exe.bak)   ← Windows 允许 rename 运行中的 exe
  3. copy(p2p-cache/新版本 → phaethon.exe)      ← 原路径空出来了
  4. exit(42)                                   ← 通知 watchdog

Watchdog（看到 worker 退出码 42）:
  5. spawn 新 watchdog（传入自己的 PID）
     → phaethon.exe --cleanup-pid=<old_watchdog_pid>
  6. 自己 exit

新 Watchdog:
  7. 检测到 --cleanup-pid 参数
  8. WaitForSingleObject(old_pid) — 等旧 watchdog 彻底退出
  9. 旧 watchdog 退出 → phaethon.exe.bak 锁释放
  10. 删除 phaethon.exe.bak
  11. 正常启动 worker（新二进制）
```

**关键点**：
- Windows 运行中的 exe 不能删除/覆盖，**可以 rename**
- Worker 在 exit(42) 前完成磁盘文件替换
- Watchdog 看到 42 不会重启 worker，而是启动新的 watchdog 并退出
- 新 watchdog 等旧进程退出后再清理 .bak，确保文件锁已释放
- 交接期间监控不断档 — 旧 watchdog 确认新 watchdog 启动后才退出

---

## 6. 关键设计决策

| 问题 | 决策 | 理由 |
|------|------|------|
| P2P 连接方式 | **PORT=2 独立通道** | 与控制面、数据面物理隔离，失败静默跳过 |
| 二进制分发模式 | **库存式分发** | 每个节点缓存多平台二进制，像种子一样扩散 |
| 版本比较 | **解析 git describe 格式** | 支持 tag、tag+N commits、hash、dev |
| 平台兼容性检测 | **运行时检测 Windows 版本** | 不依赖编译参数，RtlGetVersion 自动识别 Win7 |
| 更新触发 | **版本比较，低版本请求同步** | 对等设计，不区分 client/server |
| Linux 自更新 | **execve() 进程内替换** | 同一 PID，watchdog 无感知，无缝交接 |
| Windows 自更新 | **exit(42) + watchdog 交接** | Windows 无 execve，通过退出码协调 watchdog 接力 |
| .bak 清理 | **新进程启动时清理** | 旧进程持有文件锁，必须等新进程处理 |
| ResolveCmd | **已删除** | dstPort 启发式多余，各方法显式指定命令 |
| 重连策略 | **指数退避（1s→60s）** | 避免快速重试加剧故障 |

---

## 7. 实现方案

### 7.1 已完成

| 阶段 | 内容 | 状态 |
|------|------|------|
| 基础通道 | BindPortP2P=2、删除 ResolveCmd、PORT=2 路由、DialP2P | ✅ |
| P2P 模块骨架 | P2PManager、HandleP2PConn、StartPeer、心跳 | ✅ |
| 版本交换 | hello 命令、版本比较、Admin API `/api/p2p` | ✅ |
| 二进制传输 | manifest、chunk_req/chunk_data、逐片哈希校验 | ✅ |
| 健壮性 | 指数退避重连、平台不匹配处理 | ✅ |

### 7.2 待实现

| 阶段 | 内容 |
|------|------|
| 库存式分发 | 缓存目录管理、清单交换、多平台同步 |
| 自更新（Linux） | execve() 进程内替换 |
| 自更新（Windows） | exit(42) + watchdog 交接 + .bak 清理 |
| 运行时检测 | detectBuildTag() Windows 版本检测 |

---

## 8. 文件清单

| 文件 | 操作 | 内容 |
|------|------|------|
| `reverse/control.go` | 修改 | 新增 `BindPortP2P = 2` |
| `dialer/dialer.go` | 修改 | 删除 `ResolveCmd`、`IsBind`；新增 `P2PDialer` 接口 |
| `dialer/socks5.go` | 修改 | `Dial()` 硬编码 CONNECT；新增 `DialP2P()` |
| `dialer/trojan.go` | 修改 | `Dial()` 硬编码 CONNECT；新增 `DialP2P()` |
| `dialer/htunnel.go` | 修改 | 提取 `dialHTunnel()` 共享方法；新增 `DialP2P()` |
| `server/socks5.go` | 修改 | BIND 路由增加 PORT=2 分支 |
| `server/trojan.go` | 修改 | BIND 路由增加 PORT=2 分支 |
| `server/htunnel.go` | 修改 | BIND 路由增加 PORT=2 分支 |
| `server/p2p_server.go` | **新建** | 委托到 p2p.HandleP2PConnection |
| `p2p/p2p.go` | **新建** | P2P 管理器、版本交换、重连 |
| `p2p/transfer.go` | **新建** | 分片传输、哈希校验 |
| `p2p/version_test.go` | **新建** | 版本比较测试 |
| `p2p/detect_windows.go` | **新建** | Windows 运行时版本检测 |
| `p2p/detect_other.go` | **新建** | 非 Windows 平台 stub |
| `main.go` | 修改 | 初始化 P2PManager，启动 P2P peers |
| `admin/admin.go` | 修改 | 新增 `/api/p2p` 接口 |

---

## 9. 风险与回退

| 风险 | 影响 | 缓解 |
|------|------|------|
| 传输中连接断开 | 中 | 临时文件不替换，等完整接收+校验后才存入缓存 |
| 新旧版本不兼容 | 高 | .bak 备份保留，新进程启动后清理 |
| 平台不同误传 | 低 | hello 中携带 platform/arch/buildTag，三者匹配才同步 |
| 循环更新（A→B→A） | 中 | 版本相同时不触发同步 |
| Windows 运行中 exe 不能覆盖 | 中 | rename 旧的 → .bak，写入新的到原路径 |
| Windows .bak 被 watchdog 锁住 | 中 | 新 watchdog 等旧进程退出后再删 |
| Windows watchdog 交接空窗期 | 低 | 旧 watchdog 确认新 watchdog 启动后才退出 |
| 缓存目录磁盘占用 | 低 | 后续可加上限策略（每平台保留最近 N 个版本） |

---

## 10. 验收标准

- [ ] SOCKS5/Trojan/h_tunnel 代理自动建立 PORT=2 P2P 连接
- [ ] Admin API 可查看 P2P peer 状态（版本、平台、库存）
- [ ] 节点间交换库存清单，自动同步缺失或低版本的二进制
- [ ] 每块数据校验 SHA-256 哈希
- [ ] 整体文件校验 SHA-256 哈希
- [ ] 匹配自己架构且版本更高的二进制触发自动替换
- [ ] Linux: execve() 进程内替换，watchdog 无感知
- [ ] Windows: exit(42) → watchdog 交接 → 新 watchdog 清理 .bak → 启动新 worker
- [ ] 运行时检测 Windows 版本（Win7 自动标记 "win7"）
- [ ] 版本比较正确处理 git describe 格式
- [ ] P2P 连接断开后指数退避重连

---

## 11. 相关链接

- Spec: [reverse_spec.md](../specs/reverse_spec.md)
- Spec: [protocol_spec.md](../specs/protocol_spec.md)
- Plan: [reverse_control_channel_design.md](reverse_control_channel_design.md)
- Plan: [unified_frame_protocol_design.md](unified_frame_protocol_design.md)
