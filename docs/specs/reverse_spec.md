# reverse_spec.md

## 元数据

- 文档类型：Spec
- 版本：v0.1.0
- 所属项目：phaethon
- 创建日期：2026-07-14

## 变更记录

| 版本 | 日期 | 变更内容 | 作者 |
|------|------|----------|------|
| v0.1.0 | 2026-07-14 | 初始版本：反向连接与 Registry 规格 | Claude |

## 1. 概述

反向连接用于内网穿透：被动端（内网）主动向外建立连接并注册到注册端；主动端（公网）从注册端匹配这些连接来传输数据。

## 2. 四角色拓扑

```
客户端 → 注册端 → 反向端 → 服务端
```

每两个角色之间的链路都可能经过多级代理，由 `dialer.ChainDial` 处理。

## 3. 统一反向连接帧协议

反向连接控制消息与数据共用统一帧协议，格式为：

```
TYPE(1B) + LENGTH(2B, big-endian) + PAYLOAD
```

### 3.1 帧类型

| TYPE | 名称 | 方向 | 说明 |
|------|------|------|------|
| `0x01` | HEARTBEAT | 双向 | 空 payload，30s 间隔 |
| `0x02` | PONG | 注册端→反向端 | 匹配成功通知 |
| `0x03` | PENG | 反向端→注册端 | 注册确认/握手完成 |
| `0x04` | UDP_CHANNEL | 注册端→反向端 | UDP 隧道建立命令 |
| `0x05` | DATA | 双向 | 原始应用层数据 |

### 3.2 生命周期

1. 反向端通过代理链建立到注册端的 TCP 连接。
2. 反向端发送 `PENG`（数据连接）或进入控制处理循环（控制连接）。
3. 注册端将数据连接放入 `Registry`。
4. 主动端请求到达后，注册端从 `Registry.Match(address)` 获取连接并发送 `PONG`。
5. 反向端收到 `PONG` 后回复 `PENG`，握手完成，进入数据转发。
6. 任意方向 60s 无帧则判定链路死亡。

## 4. Registry

### 4.1 核心 API

```go
reverse.Refresh()                       // 刷新注册表，关闭旧未匹配连接，取消等待者
reverse.GlobalRegistry() *Registry      // 获取当前全局注册表
registry.Register(address string, mc *ManagedConn)   // 被动端注册连接
registry.Match(address string) (*ManagedConn, error) // 主动端匹配连接
```

### 4.2 ManagedConn

`ManagedConn` 包装 `net.Conn`，提供：

- 独立 read loop，按帧协议读取并过滤心跳。
- 线程安全的 `WriteMsg`。
- `Stop()` 安全退出 read loop，不关闭底层连接。
- 心跳 sender 管理。

## 5. 动态控制通道（可选扩展）

支持反向端通过控制链路向注册端动态申请资源：

- 控制连接：BIND PORT=1。
- 数据连接：BIND PORT=0，address 由注册端分配。
- 控制消息通过 `FrameData` 承载 JSON 命令（`register` 等）。
- 注册端收到请求后动态创建 listener、proxy 和路由规则；控制链路断开时自动清理。

详见 [dynamic-reverse-control-channel.md](../../dynamic-reverse-control-channel.md)。

## 6. 配置示例

### 6.1 被动端（内网）

```yaml
mappings:
  - name: RV_OUT
    type: socks5
    reverse-address: my-node
    reverse-proxy: OUTBOUND_PROXY
    reverse-max-connections: 3
```

### 6.2 主动端（公网）

通过配置带 `reverse-address` 的 proxy，在 Match 时从 Registry 获取已注册连接：

```yaml
proxies:
  - name: MY_RV
    type: socks5
    reverse-address: my-node
```

将目标规则指向 `MY_RV`，流量即可通过 `my-node` 的反向连接转发到内网。

## 7. 相关链接

- [dynamic-reverse-control-channel.md](../../dynamic-reverse-control-channel.md)
- [unified-frame-protocol.md](../../unified-frame-protocol.md)
- [protocol_spec.md](protocol_spec.md)
