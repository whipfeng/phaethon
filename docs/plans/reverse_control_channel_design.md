# 动态反向控制通道设计文档

## 1. 背景与目标

### 现状

当前 phaethon 的所有配置（监听端口、协议类型、代理链、路由规则）都**静态写在 YAML 配置文件中**。每次部署都需要手动编写配置文件，在需要管理大量反向端时效率低下。

### 目标

实现 **零配置分发**：一个二进制文件 + 内嵌默认配置即可运行。反向端通过控制通道向注册端动态申请资源，注册端自动完成端口监听、代理创建和路由配置。

### 适用场景

- 大量 NAT 后的设备（反向端）需要统一接入
- 设备动态上下线，无法预先写死配置
- 需要远程批量管理入口端口和路由规则

---

## 2. 四角色拓扑

```
客户端 ──→ 注册端 ──→ 反向端 ──→ 服务端
  │          │          │         │
  │    [可能经   [可能经   [可能经
  │     多级代理] 多级代理] 多级代理]
  │          │          │         │
  │    协议解析   反向连接   真实目标
  │   动态路由   Registry   (网站/API)
```

| 角色 | 定义 | 职责 |
|------|------|------|
| **客户端** | 用户（curl/浏览器等） | 发起请求 |
| **注册端** | 公网入口 | 监听端口、协议解析、Registry 管理、动态资源分配 |
| **反向端** | NAT 后设备 | 通过代理链出站，申请资源，转发数据到服务端 |
| **服务端** | 最终目标 | 提供真实服务（网站、API 等） |

每两个角色之间的链路都可能经过多级代理（Trojan → SOCKS5 → ...），由 `ChainDial` 处理。

---

## 3. 核心架构：双链路分离

```
┌────────────────────────────────────────────────────────────┐
│                    控制链路（长连接）                         │
│                                                            │
│   反向端 ──[register 请求]──→ 注册端                        │
│            ←──[address + port]──                           │
│                                                            │
│   注册端收到请求后自动执行：                                  │
│     ① 在 port 上启动指定协议的 Listener                     │
│     ② 创建 Proxy（ReverseAddress = 分配的唯一 ID）           │
│     ③ 插入路由规则（该端口流量 → 该 Proxy）                  │
│                                                            │
├────────────────────────────────────────────────────────────┤
│                    数据链路（按需建立）                       │
│                                                            │
│   客户端 → 注册端:port                                      │
│          → 协议解析（SOCKS5 / Trojan / Direct）             │
│          → 路由规则匹配 → 命中该端口的专用 Proxy              │
│          → Registry.Match(address) → 拿到反向端的数据连接     │
│          → 反向端 → 服务端                                   │
│                                                            │
└────────────────────────────────────────────────────────────┘
```

**关键设计原则：控制和数据分离**
- 控制链路只传输信令（申请、心跳、状态），不传业务数据
- 数据链路按需建立，每条客户端连接独立占用一条反向连接
- 控制断开则数据全部回滚

---

## 4. 控制与数据连接的区分方式

### 决策：复用帧协议 + BIND PORT 字段区分角色

**不新增帧类型。** 控制消息和数据消息都通过现有帧协议传输，
区别在于两条物理上独立的 TCP 连接，通过 SOCKS5/Trojan BIND 命令的 PORT 字段声明角色：

| BIND PORT | 含义 | 注册端行为 |
|-----------|------|-----------|
| `0` | 数据连接（现有默认行为） | 入 Registry 等待 PONG 匹配 |
| `1` | 控制连接（新增） | 不入 Registry，进入控制处理循环 |

### 为什么选这个方案

1. **必须用帧协议而非 HTTP** — 控制链路和数据链路一样要穿透代理链（Trojan → SOCKS5 → ...），最终都是在一条 `net.Conn` 上跑协议。HTTP 的调试优势在代理链场景下不存在
2. **不新增帧类型** — 不需要在帧头层面做区分；控制和数据是两条不同的 conn，天然隔离
3. **复用被忽略的 PORT 字段** — SOCKS5/Trojan 的 BIND 处理中都读取了 DST.PORT 但从未使用（只传了 DST.ADDR 给 `HandleReverseConnection`）。PORT=1 作为控制标识零成本
4. **语义清晰** — 反向端建连时主动声明"我是控制连接"还是"我是数据连接"

### 连接建立流程

```
反向端                                    注册端
  │                                         │
  │ ═══ 控制连接 ═══                          │
  │                                         │
  │ ChainDial(代理链) → 连到注册端 listener    │
  │     ↓                                    │
  │ SOCKS5/Trojan 握手                       │
  │     ↓                                    │
  │ CMD=BIND(address="reg-endpoint", PORT=1) │  ← 声明是控制连接
  │     ↓                                    │
  │                    注册端判断 dstPort==1  │
  │                         ↓               │
  │                    handleControlConn()   │  ← 不入 Registry
  │                         ↓               │
  │              进入心跳循环 + 控制命令收发    │
  │                                         │
  │ ═══ 数据连接（每条客户端连接对应一条）═══   │
  │                                         │
  │ ChainDial(代理链) → 连到注册端 listener    │
  │     ↓                                    │
  │ SOCKS5/Trojan 握手                       │
  │     ↓                                    │
  │ CMD=BIND(address="dyn-a1b2c3d4", PORT=0) │  ← 默认，数据连接
  │     ↓                                    │
  │                 注册端判断 dstPort==0      │
  │                      ↓                   │
  │           HandleReverseConnection()       │  ← 入 Registry
  │                      ↓                   │
  │            心跳循环，等待 PONG 匹配        │
```

---

## 5. 完整流程

### 5.1 启动阶段

```
1. 注册端启动
   ├── 加载内嵌默认配置（或 conf/rule.yaml）
   ├── 开启入站 listener（SOCKS5 或 Trojan，复用现有机制）
   ├── 初始化 ControlManager（控制连接管理器）
   └── 进入就绪状态

2. 反向端启动
   ├── 加载内嵌默认配置
   ├── 通过代理链出站连到注册端的入站 listener
   ├── 发送 BIND(PORT=1) 建立控制连接
   └── 发送 register 请求
```

### 5.2 资源申请阶段

```
反向端                              注册端
  │                                   │
  │ 在控制连接上发 FrameData(JSON):    │
  │ {                                 │
  │   "cmd": "register",              │
  │   "proto": "socks5",              │  ← 反向端连注册端用的协议
  │   "preferred_port": 19902,        │  ← 期望监听端口
  │   "listener_proto": "socks5",     │  ← 动态监听的协议类型
  │   "listener_user": "u1",          │  ← 监听认证用户名
  │   "listener_password": "p1",      │  ← 监听认证密码
  │   "listener_sni": "example.com",  │  ← Trojan/SNI
  │   "direct_dst_host": "10.0.0.1",  │  ← direct 类型目标地址
  │   "direct_dst_port": 8080,        │  ← direct 类型目标端口
  │   "tcp_port_range": "20000-20100" │  ← 动态监听端口范围
  │ }                                 │
  │ ──────────────────────────────→  │
  │                                   │  生成 UUID: "dyn-a1b2c3d4"
  │                                   │  动态资源配置：
  │                                   │  ① net.Listen("tcp", ":19902")
  │                                   │  ② 按 listener_proto 启动对应 Server
  │                                   │     (SOCKS5/Trojan/Direct，带认证)
  │                                   │  ③ 创建 Proxy(REVERSE, addr="dyn-a1b2c3d4")
  │                                   │  ④ 插入路由规则(该端口流量 → 该 Proxy)
  │                                   │
  │ ←── FrameData(JSON 回复) ────────  │
  │ {                                 │
  │   "status": "ok",                 │
  │   "address": "dyn-a1b2c3d4",      │  ← 用于后续数据 BIND
  │   "port": 19902                   │  ← 实际监听端口（信息用途）
  │ }                                 │
```

### 5.3 TCP 数据转发阶段

```
反向端                                注册端                          客户端
  │                                     │                               │
  │ ReverseServer 自动管理连接池         │                               │
  │ ├─ BIND(PORT=0, "dyn-a1b2c3d4")    │                               │
  │ ├─ BIND(PORT=0, "dyn-a1b2c3d4")    │                               │
  │ └─ BIND(PORT=0, "dyn-a1b2c3d4")   │                               │
  │     ↓                               │                               │
  │  ───────────→ 3条数据连接入 Registry │                               │
  │                                     │                               │
  │                                     │ ←── SOCKS5 CONNECT ──────     │
  │                                     │    target: www.example.com:443 │
  │                                     │                               │
  │                                     │ 规则匹配 → Proxy(dyn-p1)      │
  │                                     │ → Registry.Match("dyn-a1..") │
  │                                     │ → 拿到反向端的数据连接         │
  │                                     │  → PONG 匹配 → 交给 Handler   │
  │                                     │                               │
  │ ←── FrameData ────────────────────→ │ ←── Relay ──────────────────→  │
  │  解析目标                           │  转发到服务端                   │
  │  拨 www.example.com:443           │                               │
```

**反向端复用现有 ReverseServer 的能力：**
- 连接池管理（maxConns、自动补充断开连接）
- PONG/PENG 握手匹配
- 心跳保活（FrameHeartbeat）
- TCP/UDP 双路径自动分路（收到 FrameUDPChannel → handleReverseUDPChannel）

反向端只需：
1. 控制连接注册成功后得到 `address`
2. 构造临时 `Proxy`（Type=register_proto, Server=注册端, Next=outbound_proxy）
3. 构造临时 `Mapping`（ReverseAddress=address, ReverseProxy=tempProxy.Name）
4. 调用 `StartReverseMapping(ruleConf, mapping)` → 返回的 `*ReverseServer` 接管一切

### 5.4 UDP 数据转发阶段

```
客户端 → 注册端:19902 (SOCKS5 UDP ASSOC)
       → 路由规则命中 Proxy(dyn-p1, addr="dyn-001")
       → ReverseDialer.DialPacket()
       → Registry.Match("dyn-001")
       → 拿到反向端的数据 TCP 连接
       → 发送 FrameUDPChannel(0x04) 建加密 UDP 隧道
       → 反向端解密后转发到服务端(UDP)

同一个 address "dyn-001" 同时服务于 TCP 和 UDP。
TCP 走 ChainDial + Relay，UDP 走 DialPacket + FrameUDPChannel 隧道。
dialer 层自动根据调用方法选择路径，注册端无需区分。
```

### 5.5 断开清理阶段

```
控制链路断开（网络中断 / 进程退出 / 心跳超时）

注册端自动执行（ControlManager 清理）：
  ① 关闭动态 Listener (:19902)
  ② 删除动态 Proxy (dyn-p1)
  ③ 删除动态路由规则
  ④ 通过 Registry.CloseByAddress 关闭所有 address="dyn-a1b2c3d4" 的数据连接
  ⑤ 释放端口资源

反向端自动执行（Graceful Shutdown）：
  ① `ReverseServer.Close()` 设置 `closed=true`，关闭 `closeCh`
  ② 停止创建**新的**数据反向连接
  ③ 已有活跃连接**不强制断开**，允许自然结束（由注册端侧清理）
  ④ 等待 reconnect-interval 后重新发起控制连接注册

> **为什么活跃连接不强制断开？** 注册端在检测到控制通道断开后会主动 `CloseByAddress`，
> 数据连接会自然关闭。反向端强制 `Close()` 会导致客户端正在进行的请求被异常中断。
> 反向端只做"停止接受新连接"，不做"踢掉现有连接"。
```

---

## 6. 控制消息格式

控制连接建立后，通过 `FrameData` (0x05) 传输 JSON 格式的控制消息。
心跳使用 `FrameHeartbeat` (0x01)，复用现有机制。

### 6.1 注册请求（反向端 → 注册端）

```json
{
  "cmd": "register",
  "proto": "socks5",
  "preferred_port": 19902,
  "listener_proto": "socks5",
  "listener_user": "user",
  "listener_password": "pass",
  "listener_sni": "example.com",
  "direct_dst_host": "10.0.0.1",
  "direct_dst_port": 8080,
  "tcp_port_range": "20000-20100"
}
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `cmd` | string | 是 | 固定值 `"register"` |
| `proto` | string | 是 | 反向端连注册端用的协议：`socks5` / `trojan` / `h_tunnel` |
| `preferred_port` | int | 否 | 期望监听端口，0 = 自动分配 |
| `listener_proto` | string | 否 | 动态监听的协议：`socks5` / `trojan` / `direct`，默认 `socks5` |
| `listener_user` | string | 否 | 监听协议用户名（SOCKS5 认证） |
| `listener_password` | string | 否 | 监听协议密码（SOCKS5/Trojan 认证） |
| `listener_sni` | string | 否 | Trojan 监听的 SNI |
| `direct_dst_host` | string | 否 | `direct` 类型监听的目标地址 |
| `direct_dst_port` | int | 否 | `direct` 类型监听的目标端口 |
| `tcp_port_range` | string | 否 | 动态监听端口范围，格式 `"min-max"`（如 `"20000-20100"`）。`preferred_port` 不在范围内时自动从范围分配 |

**字段设计原则：**
- `proto` 是**反向端→注册端**的控制通道协议（必须支持 BIND，只能是 socks5/trojan/h_tunnel）
- `listener_proto` 是**注册端对外监听**的协议（客户端连注册端时看到的协议），可以是 socks5/trojan/direct
- `listener_*` 和 `direct_*` 字段传递给注册端用于创建带认证的动态 listener，反向端无需关心具体实现

### 6.2 注册回复（注册端 → 反向端）

```json
{
  "status": "ok",
  "address": "dyn-a1b2c3d4",
  "port": 19902
}
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `status` | string | `"ok"` 或 `"error"` |
| `address` | string | 分配的唯一 ID，用于后续数据链路 BIND 目标 |
| `port` | int | 实际监听端口（仅信息用途，反向端打印日志用） |
| `error` | string | 失败原因（仅 status=error 时有值） |

### 6.3 心跳

```
方向：双向
间隔：30 秒（复用 FrameHeartbeat，空 payload）
超时判定：60 秒未收到任何帧 → 判定控制链路断开 → 触发清理
```

---

## 7. 与静态配置对比

| 维度 | 静态配置（当前） | 动态控制通道（新） |
|------|------------------|-------------------|
| **Listener 创建时机** | 进程启动时从 YAML 读取 | 收到 register 命令后动态创建 |
| **Proxy 定义** | YAML `proxies:` 字段写死 | 运行时动态生成对象 |
| **路由规则** | YAML `rules:` 字段预定义 | 运行时动态插入 RuleConfiguration |
| **地址来源** | 配置中预定义字符串 | 注册端分配 UUID 式唯一 ID |
| **生命周期** | 跟随进程，修改需改文件 + 热加载 | 跟随控制链路，断开即销毁 |
| **适用场景** | 固定拓扑、少量节点 | 动态拓扑、大量节点、零配置分发 |

---

## 8. 多反向端并发场景

```
反向端 #1 ──BIND(PORT=1, 控制连接)────→ 注册端
            ──register(socks5, :19902)──→
            ←──{address:"dyn-001", :19902}─
            ──BIND("dyn-001", PORT=0)───→ 数据连接入Registry["dyn-001"]

反向端 #2 ──BIND(PORT=1, 控制连接)────→ 注册端
            ──register(trojan, :19903)───→
            ←──{address:"dyn-002", :19903}─
            ──BIND("dyn-002", PORT=0)───→ 数据连接入Registry["dyn-002"]

反向端 #3 ──BIND(PORT=1, 控制连接)────→ 注册端
            ──register(socks5, :19902)───→  端口冲突
            ←──{address:"dyn-003", :19904}─  自动递增
            ──BIND("dyn-003", PORT=0)───→ 数据连接入Registry["dyn-003"]
```

**路由天然隔离**：每个反向端分配独立的 address，Registry 按 key 分池，
`Match("dyn-001")` 只能拿到反向端 #1 的连接，不会串。

---

## 9. 已确定的决策记录

| 问题 | 决策 | 理由 |
|------|------|------|
| 控制链路协议 | 复用现有帧协议 | 必须穿透代理链，HTTP 调试优势在代理链场景下不存在 |
| 控制/数据区分方式 | BIND PORT 字段（0=数据，1=控制） | 不新增帧类型，PORT 字段原本就被忽略，零成本 |
| 是否新增帧类型 | 不需要 | 两条物理 conn 天然隔离，不需要在协议层标记 |
| 控制消息载体 | FrameData + JSON payload | 控制连接上的应用层数据就是 JSON 命令 |
| 心跳机制 | 复用 FrameHeartbeat | 30s/60s 现有机制直接用 |
| 地址分配方式 | 注册端生成 UUID | 反向端无需协调，全局唯一 |
| 端口作用 | 对注册端=实际绑定端口；对反向端=纯信息 | 反向端数据连接不用这个端口，用分配的 address 做 BIND |
| 反向端能否直连注册端 | **不能**，必须走代理链 | 反向端通常在 NAT/防火墙后，直连无法穿透；统一要求 outbound proxy 简化逻辑 |
| 数据连接池管理 | 复用 `ReverseServer` | 已有完整实现（连接池、PONG/PENG、心跳、自动补充、TCP/UDP 双路径），不重复造轮子 |
| 断开清理策略 | Graceful shutdown | `ReverseServer.Close()` 只停止新连接，已有连接由注册端 `CloseByAddress` 关闭，避免中断正在进行的请求 |
| 控制通道 SOCKS5 建立 | 提取 host/port 后 `ChainDial(proxy, host, port)` | 直接传 `host:port` 给 ChainDial 会导致 DIRECT 代理下拼接成 `host:port:port` |
| UDP 反向通道 | `ReverseServer` 自动处理 | `StartReverseMapping` 的 handler 已支持 `FrameUDPChannel` → `handleReverseUDPChannel`，无需反向端额外代码 |
| 动态监听端口范围 | `tcp_port_range` 字段，格式 `"min-max"` | 与现有 `udp-port-range` 保持一致；`preferred_port` 不在范围内时自动回退到范围内分配 |
| 出站代理与注册中心的关系 | **出站代理就是注册中心** | `proxy.Server:proxy.Port` 即为注册中心地址，`proxy.Type` 即为注册协议。反向端通过出站代理连接到注册端，出站代理的服务器本身就是注册端的 listener |
| `registry-addr` 字段 | **冗余，应删除** | 注册中心地址 = `outbound-proxy` 的 `Server:Port`，无需单独存储。UI 向导已自动从代理推导，但独立字段会导致手动配置时不一致 |
| `register-proto` 字段 | **冗余，应删除** | 注册协议 = `outbound-proxy` 的 `Type`（socks5/trojan/h_tunnel）。UI 向导已自动推导（`opt.dataset.type`），独立字段多余 |
| 控制连接建立方式 | **代理多态，非 switch 分发** | `ControlClient.Connect()` 不应 switch `registerProto`。应在 `Dialer` 接口上新增 `DialControl()` 方法，各代理类型自己实现：SOCKS5 → ChainDial + BIND(PORT=1)；Trojan → nextDialer.Dial(server,port) + TLS + Trojan BIND(cmd=0x02)；HTunnel → Dial(server,1)。调用方只认一个方法 |
| BIND PORT=1 含义 | 控制连接标识 | SOCKS5/Trojan/HTunnel 的 BIND 命令中 DST.PORT=1 表示控制连接，PORT=0 表示数据连接。注册端根据此值决定走 `handleControlConn()` 还是 `HandleReverseConnection()`。复用原本被忽略的 PORT 字段，零成本 |
| `seq` 字段分配时机 | **创建反向配置时分配** | 通过 Admin UI 创建反向配置时，自动分配 `seq` 并持久化到配置文件。避免重启后 seq 变化导致注册端拒绝（注册端用 `reverseID + seq` 标识客户端） |
| `seq` 分配策略 | **找第一个空位（gap-filling）** | 不使用 `max+1`，而是找第一个未被使用的 seq 数字。删除中间配置后，新建配置会复用该空位，保持 seq 紧凑 |

---

## 10. `seq` 字段设计

### 10.1 背景

注册端使用 `reverseID + seq` 作为客户端标识。反向端重启后，如果 `seq` 变化，注册端会认为是不同客户端，拒绝重新注册已分配的端口。

**问题场景：**
1. 反向配置创建时未分配 `seq`（`seq=0` 因 `omitempty` 未持久化）
2. 每次重启，`normalizeReverseConfigs()` 分配新 `seq`
3. 注册端拒绝：`preferred port X unavailable: already bound to reverse identity <id>#<old_seq>`

### 10.2 解决方案

**创建时分配：** 通过 Admin UI 创建反向配置时，自动分配 `seq` 并保存到配置文件。

```go
// admin/admin.go - 创建反向配置时
if rc.Seq <= 0 {
    used := make(map[int]bool)
    for _, existing := range configs {
        if existing.Seq > 0 {
            used[existing.Seq] = true
        }
    }
    next := 1
    for used[next] {
        next++
    }
    rc.Seq = next
}
```

**Gap-filling 策略：** 不使用 `max+1`，而是找第一个空位。

| 场景 | 现有 seq | 新建分配 |
|------|----------|----------|
| 初始 | [] | 1 |
| 新增 | [1] | 2 |
| 新增 | [1, 2] | 3 |
| 删除 seq=2 | [1, 3] | 2（复用空位） |
| 新增 | [1, 2, 3] | 4 |

### 10.3 为什么不用 `max+1`

- `max+1` 会在删除中间配置后留下永久空位
- 长期使用后 seq 数字会无限增长
- Gap-filling 保持 seq 紧凑，便于调试和日志阅读

---

## 11. 出站代理内化设计（待实施）

### 11.1 核心原则

出站代理（`outbound-proxy`）就是注册中心。反向端通过出站代理连接到注册端，出站代理的服务器本身就是注册端的 listener。

```
反向端 ──通过出站代理──→ 注册端 listener（就是出站代理的 Server:Port）
```

因此：
- **注册中心地址** = `proxy.Server:proxy.Port`
- **注册协议** = `proxy.Type`（必须是支持 BIND 的类型：socks5 / trojan / h_tunnel）

`ReverseConfig` 中的 `registry-addr` 和 `register-proto` 字段冗余，应从配置中删除，改为运行时从出站代理推导。

### 11.2 多态控制连接

当前 `ControlClient.Connect()` 通过 `switch registerProto` 分发到不同协议逻辑。应改为代理多态：

```go
// Dialer 接口扩展
type ControlDialer interface {
    DialControl() (net.Conn, error)
}
```

各代理类型实现：

| 代理类型 | DialControl 实现 |
|---------|-----------------|
| SOCKS5 | `ChainDial(proxy, server, port)` → SOCKS5 BIND(PORT=1) |
| Trojan | `nextDialer.Dial(server, port)` → TLS 握手 → Trojan BIND(cmd=0x02, PORT=1) |
| HTunnel | `Dial(server, 1)` |

`ControlClient.Connect()` 简化为：

```go
d := NewDialer(c.proxy)
conn, err := d.(ControlDialer).DialControl()
```

调用方不再关心协议细节，各代理类型内化自己的控制连接逻辑。

### 11.3 配置简化

**删除前（ReverseConfig）：**
```yaml
reverse:
  - name: nicai3
    outbound-proxy: SOCKS5_7890    # 出站代理（就是注册中心）
    registry-addr: 106.13.183.103:39999  # 冗余，= proxy.Server:proxy.Port
    register-proto: trojan               # 冗余，= proxy.Type
```

**删除后：**
```yaml
reverse:
  - name: nicai3
    outbound-proxy: SOCKS5_7890    # 唯一需要的字段，其余自动推导
```

---

## 12. 文件变更清单

| 文件 | 操作 | 内容 |
|------|------|------|
| `reverse/control.go` | 修改 | `ControlRequest` 扩展 6 个字段（`listener_proto/user/password/sni` + `direct_dst_host/port`） |
| `config/config.go` | 修改 | `ReverseConfig` 新增对应 6 个 yaml 字段 |
| `server/control_server.go` | 修改 | `handleRegister` 读取新字段传给 `createListener`；`createListener` 根据 `listener_proto` + 认证信息创建对应类型 listener |
| `dialer/control_client.go` | 重写 | 移除 nil proxy 直连分支；`connectSOCKS5()` 提取 host/port 避免双重端口；`Register()` 接收完整 `ControlRequest`；`Connect()` 修复 switch 后 err 检查缺失 |
| `server/reverse.go` | 修改 | `Close()` 改为 graceful（不强制关闭已有连接）；新增 `CloseForce()` 用于进程退出时强制清理 |
| `server/socks5.go` | 修改 | BIND 处理：严格校验 PORT 只能为 0（数据）或 1（控制） |
| `server/trojan.go` | 修改 | 同上，BIND PORT 严格校验 |
| `server/htunnel.go` | 修改 | 同上，BIND PORT 严格校验 |
| `main.go` | 重写 | `startReverseClient`/`runReverseSession` 完全重写：复用 `ReverseServer` 管理数据连接池，构造临时 Proxy+Mapping 调用 `StartReverseMapping`；传递 `tcp_port_range` |
