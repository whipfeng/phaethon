# protocol_spec.md

## 元数据

- 文档类型：Spec
- 版本：v0.1.0
- 所属项目：phaethon
- 创建日期：2026-07-14

## 变更记录

| 版本 | 日期 | 变更内容 | 作者 |
|------|------|----------|------|
| v0.1.0 | 2026-07-14 | 初始版本：协议支持矩阵与通用约定 | Claude |

## 1. 概述

phaethon 的协议层分为入站（Server）和出站（Dialer）。所有 Server 实现 `ConnHandler` 接口；所有 Dialer 实现 `Dialer` 接口，可选实现 `UDPDialer`。

## 2. 协议支持矩阵

### 2.1 入站 Server

| 协议 | 文件 | TCP | UDP | 说明 |
|------|------|-----|-----|------|
| SOCKS5 | `server/socks5.go` | ✓ | ✓ | 支持用户名密码认证、UDP ASSOCIATE |
| Trojan | `server/trojan.go` | ✓ | ✓ | TLS 之上 Trojan-GFW 协议 |
| h_tunnel | `server/htunnel.go` | ✓ | ✓ | HTTP 隧道协议 |
| HTTP | `server/http.go` | ✓ | — | HTTP 代理入站 |
| HTTPS | `server/http.go` | ✓ | — | HTTPS 代理（自签名证书） |
| Direct | `server/direct.go` | ✓ | — | 直接转发到 `dst-host:dst-port` |
| Reverse | `server/reverse.go` | ✓ | — | 反向连接服务端 |

### 2.2 出站 Dialer

| 协议 | 文件 | TCP | UDP | 说明 |
|------|------|-----|-----|------|
| DIRECT | `dialer/direct.go` | ✓ | ✓ | 直连 |
| SOCKS5 | `dialer/socks5.go` | ✓ | ✓ | SOCKS5 客户端 |
| Trojan | `dialer/trojan.go` | ✓ | ✓ | Trojan 客户端 |
| h_tunnel | `dialer/htunnel.go` | ✓ | ✓ | HTTP 隧道客户端 |
| VLESS | `dialer/vless.go` | ✓ | ✓ | VLESS 客户端 |
| Hysteria2 | `dialer/hysteria2.go` | ✓ | ✓ | Hysteria2 客户端 |
| Shadowsocks | `dialer/shadowsocks.go` | ✓ | ✓ | Shadowsocks 客户端 |
| SSH | `dialer/ssh.go` | ✓ | — | SSH 动态端口转发 |
| HTTP | `dialer/http.go` | ✓ | — | HTTP 代理客户端 |
| REVERSE | `dialer/reverse.go` | ✓ | ✓ | 从 Registry 获取反向连接 |

## 3. 通用约定

### 3.1 Server 实现模式

1. 定义 `XxxServer` 嵌入 `BaseServer`。
2. 实现 `HandleConn(net.Conn)` 处理协议握手。
3. 用 `AcceptLoop(listener, handler, name)` 启动监听循环。
4. 解析目标地址后调用 `RuleConf.Match()` 选择代理。
5. 用 `dialer.ChainDial()` 建立出站连接。
6. 用 `util.Relay()` 或 `util.RelayWithRateLimit()` 双向转发。

### 3.2 Dialer 实现模式

1. 定义 `XxxDialer` 嵌入 `BaseDialer`。
2. 实现 `Dial(dstAddr, dstPort) (net.Conn, error)`：先连接代理服务器，再发送协议特定握手。
3. 如需 UDP 支持，实现 `UDPDialer` 接口：`DialPacket() (net.PacketConn, error)`。

### 3.3 代理链

出站拨号自动跟随 `proxy` 字段构建代理链。统一使用 `dialer.ChainDial(proxy, dstAddr, dstPort)` 递归处理。

```yaml
proxies:
  - name: A
    type: socks5
    server: 10.0.0.1
    port: 1080
    proxy: B
  - name: B
    type: trojan
    server: 10.0.0.2
    port: 443
```

请求路径：客户端 → A → B → 目标。

## 4. UDP 支持说明

- SOCKS5/Trojan/h_tunnel/VLESS/Hysteria2/Shadowsocks/DIRECT 均支持 UDP ASSOCIATE / UDP relay。
- UDP relay 生命周期应跟随控制连接（TCP/TLS/h_tunnel channel），控制连接断开时统一清理。
- 不应在 Write 路径上额外加超时，依赖 TCP 栈自身的重传超时。

## 5. 新增协议指南

新增入站协议：

1. 在 `server/` 新增 `xxx.go`。
2. 定义 `XxxServer` 嵌入 `BaseServer`。
3. 实现 `HandleConn`。
4. 在 `config.go` 或 `mapping` 解析处注册类型。

新增出站协议：

1. 在 `dialer/` 新增 `xxx.go`。
2. 定义 `XxxDialer` 嵌入 `BaseDialer`。
3. 实现 `Dial`，可选实现 `UDPDialer`。
4. 在 `NewDialer` 工厂中注册类型。

## 6. 相关链接

- [reverse_spec.md](reverse_spec.md)
- [core_spec.md](core_spec.md)
