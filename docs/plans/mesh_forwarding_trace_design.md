# Mesh 转发关联追踪设计

> 版本：0.1.0
> 状态：ACTIVE

## 1. 目标

为 QG → JF → GG 的数据面诊断提供默认关闭的关联日志，不改变任何转发、排队、重传或协议语义。

## 2. 配置

```yaml
mesh:
  forwarding-trace: false
```

关闭时不输出新增追踪事件。开启时仅追踪 IPv4 初始 TCP SYN 与 DNS UDP/53。

## 3. 描述符与关联

共享描述符只读取 IPv4/TCP/UDP 头。TCP 的关联键为 `tcp:<dst-ip>:<dst-port>:<tcp-seq>`；DNS 的关联键包含目的地址、目的端口和 DNS transaction ID。源地址、源端口、TTL、校验和均为观察字段，不作为跨阶段键，因为 NAT 可改变它们。

包内容不写入日志、不复制到持久状态、不修改原始字节。

## 4. 事件边界

| 阶段 | 位置 | 事件 |
|---|---|---|
| TUN 入站 | `tun/engine.go` | `tun_preinject` |
| 路由决定 | `mesh/netstack.go` | `route_decision` |
| Link NIC | `mesh/link_nic.go` | `linknic_egress` / `linknic_egress_error` |
| Peer 选择 | `mesh/mesh.go` | `peer_selected` / `peer_send_error` |
| P2P 写入 | `p2p/p2p.go` | `p2p_enqueued` / `p2p_queue_drop` / `p2p_write_*` |
| P2P 接收 | `p2p/p2p.go` | `p2p_received` / `p2p_inbound_*` |
| Mesh 注入 | `mesh/mesh.go`、`mesh/netstack.go` | `mesh_inject` / `linknic_inject` |

RouteSelector 只收到目的地址，因此 `route_decision` 以目的地址和时间窗关联，不伪造包级关联键。

## 5. 判据

- `tun_preinject` 后无 `linknic_egress`：本地栈路由或 Link NIC handoff。
- `linknic_egress` 后无 P2P 写入：peer 选择或队列。
- 发送端 `p2p_write_ok` 后无远端 `p2p_received`：底层 FrameTransport（包括 h_tunnel v1 BIND stream）。
- 远端收到后无 `linknic_inject`：入站 P2P/mesh handler。

## 6. ADR：只观察，不改变投递语义

保留 `FrameMeshPacket` 的尽力而为语义。追踪不得创建 ACK、重试、缓存、排序、路由回退或连接重建；日志写入不得阻塞队列与转发热路径。
