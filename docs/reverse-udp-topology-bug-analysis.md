# 反向 UDP E2E 测试 — 实际拓扑时序图

```mermaid
sequenceDiagram
    participant C as 客户端<br/>Client
    participant E as 代理端<br/>Entry<br/>SOCKS5+Trojan
    participant R as 反向端<br/>RevSide
    participant M as 中间链<br/>Middle<br/>SOCKS5 Proxy
    participant T as 服务端<br/>Target<br/>(Stub Echo)

    Note over C,T: ═════════ 第一阶段：TCP 控制通道建立 ═════════

    E->>E: 🔷 SOCKS5 监听 TCP:49164
    E->>E: 🔷 Trojan 监听 TLS:49153
    M->>M: 🔷 SOCKS5 监听 TCP:49152
    T->>T: 🔷 Echo 绑定 UDP:0

    C->>E: ① TCP CONNECT(目标地址)
    E->>R: ② Match → 反向代理<br/>(Registry 转发)
    R->>M: ③ SOCKS5 CONNECT<br/>(经中间链穿透网络)
    M->>M: ④ 转发到目标
    M-->>R: 连接成功
    R->>E: ⑤ TLS 连接代理端 Trojan
    R->>E: ⑥ BIND 注册反向地址
    E->>R: ✅ serve() 启动反向 SOCKS5

    Note over R: 规则匹配: MATCH → Middle<br/>⚠️ 仅对 TCP CONNECT 生效

    Note over C,T: ═════════ 第二阶段：UDP 数据通道建立 ═════════

    C->>E: ⑦ UDP ASSOCIATE
    E->>E: ⑧ ListenUDP() → 端口 62152 ✅<br/>在配置范围内
    E-->>C: relayAddr:62152

    Note over C,T: ═════════ 第三阶段：UDP 数据收发 ═════════

    C->>E: ⑨ UDP 发包 (SOCKS5 帧)
    E->>R: ⑩ 解包转发 (裸 UDP)<br/>经 Trojan 隧道
    R->>R: ⑪ 收到 cmd=0x04<br/>(UDP 通道请求)
    R->>R: ⑫ ChainUDPDial(.Next)<br/>.Next = nil → DIRECT
    R->>R: ⑬ ListenUDP() → 端口 62153 ✅<br/>在配置范围内

    rect rgb(255, 235, 235)
        R->>T: ⑭ WriteTo() 原始 socket<br/>⚠️ 完全绕过 Middle!
        T-->>R: Echo 回包
    end

    R->>E: ⑮ ReadFrom()<br/>Trojan 帧解析回传
    E->>C: ⑯ 封装 SOCKS5 帧回复

    Note over C,T: ─────── TCP 控制通道持续保活 ───────
    C-->>E: 心跳保活
    E-->>R: 心跳保活
```

## 端口分配汇总

| 组件 | 端口 | 协议 | 说明 |
|------|------|------|------|
| **代理端** Entry | `62152` ✅ | UDP | ListenUDP，在 [62152~62163] 范围内 |
| **反向端** RevSide | `62153` ✅ | UDP | ListenUDP，在 [62152~62163] 范围内 |
| **中间链** Middle | `N/A` | — | 不在 UDP 数据路径中 |
| **代理端** Entry | `49164` | TCP | SOCKS5 控制通道 |
| **中间链** Middle | `49152` | TCP | SOCKS5 控制通道（仅 TCP 使用）|
| **代理端** Entry | `49153` | TLS | Trojan 控制通道 |
| **服务端** Target | `62153` | UDP | Echo 桩服务（系统分配，非受控）|

## 关键发现

```
⚠️ 中间链 Middle 仅参与 TCP 控制路径（步骤 ③⑧）
   UDP 数据在步骤 ⑭ 直接用原始 socket 发往服务端，完全绕过 Middle

   原因: handleReverseUDPChannel() 调用 ChainUDPDial(reverseProxy.Next)
         reverseProxy = LOCAL_TJ (Trojan)，其 .Next = nil
         Init() 时解析为 DIRECT → 原始本地 socket (ListenUDP)
         该调用不经过规则匹配引擎，因此跳过了 Middle 配置
```
