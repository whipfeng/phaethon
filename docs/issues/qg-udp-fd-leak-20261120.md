# QG 旁路网关反复失效：UDP fd 泄漏（2026-11-20）

## 现象

- QG 控制台显示 TUN 不可用；`ip addr` 没有 `tun0`；admin 443/39999 TLS 握手超时；
- 进程仍在运行（`rc-service phaethon status` = started），但日志大量：
  `dial udp ... socket: too many open files`、`[V2-LOOKUP] no peer`、
  `[V2-ROUTE] lookup failed`、`linkNIC is nil`；
- `rc-service` 重启后 TUN 恢复，过一段时间再次失效（历史反复出现）。

## 现场证据（重启前）

| 项 | 值 |
|---|---|
| `/proc/<pid>/limits` Max open files | 4095 |
| `/proc/<pid>/fd` 数量 | **4095 / 4095** |
| UDP socket（`ss -uanp`） | **4052 个**，全部 `UNCONN *%eth1:ephemeral`、无 peer |
| TCP socket | 17 LISTEN / **12 ESTAB** |

4052 个 UDP 没有对应的 4052 条 TCP 控制连接 → 不是“有人 idle 在用 SOCKS5 UDP”，
而是 **UDP socket 生命周期结束后没有被关闭**。

## 根因

**为什么 TCP 控制面不在了、UDP 还在？** 因为这些泄漏的 UDP 不是 SOCKS5 关联，本来就没有 TCP
控制面。它们的 owner 是 gVisor 的 UDP forwarder endpoint，形成的是“端点等 handler、handler
等端点”的循环：

1. `mesh/netstack.go` 的 `acceptUDP` 收到 LAN 的 UDP 首包后创建 `udp.ForwarderRequest` 端点
   `ep` 和 `gonet.UDPConn`：
   ```go
   ep, _ := r.CreateEndpoint(&wq)
   defer ep.Close()
   conn := gonet.NewUDPConn(&wq, ep)
   defer conn.Close()
   HandleUDPConn(conn, ...)   // 同步调用
   ```
2. `HandleUDPConn` → `relayUDP` 同步阻塞在 `conn.Read` / `targetConn.ReadFrom`；
3. UDP 没有 FIN/close 事件。客户端停止发包后，`relayUDP` 永远等不到数据，也等不到关闭；
4. 于是 `HandleUDPConn` 永远不返回，`acceptUDP` 里的 `defer ep.Close()` / `defer conn.Close()`
   永远不执行；
5. `relayUDP` 也不会执行 `targetConn.Close()`（在 `HandleUDPConn` 的 defer 里）。

结果：gVisor endpoint 和 handler 创建的 OS UDP socket 互相等待，永久占着 fd。现场
4052 个无连接 UDP、只有 12 条无关 TCP ESTAB，完全符合这条路径。

**SOCKS5 UDP ASSOCIATE** 是另一条路径：它有 TCP 控制连接，正常应由控制连接 EOF 触发
`relay.Close()`；但控制连接还活着、长期无 UDP 时也没有空闲超时，属于次要泄漏点。

**fd 上限 4095 偏低**：泄漏累积到上限后，TUN 创建/重建、DNS、admin TLS 全部失败，
表现为“TUN 不可用/旁路网关失效”，并非进程崩溃。

## 修复

- `relayUDP`：增加 5 分钟空闲 watchdog（每 30s 检查），空闲后关闭 netstack 连接和 targetConn。
  这打破了第 2–5 步的循环：`relayUDP` 返回 → `HandleUDPConn` 返回 → `acceptUDP` 的 defer
  关闭 endpoint 和 conn → OS UDP socket 释放。这是补上 UDP 缺失的生命周期，不是只关 socket。
- SOCKS5 UDP relay：增加 5 分钟空闲 watchdog；client 数据报、下游回包、early reply
  都刷新活跃时间；空闲后关闭 UDP socket 和 TCP 控制连接。
- 建议 QG OpenRC 启动前 `ulimit -n 65535` 作为兜底（运维项）。

## 验证

- 单测/构建：`go build ./...`、`go test ./server ./handler` 通过；
- 部署后观察：
  - `ss -uanp | grep phaethon | wc -l` 不再持续增长；
  - `/proc/<pid>/fd` 稳定，不再逼近 4095；
  - `ip -br addr` 持续存在 `tun0`，`/api/tun` 保持 `available=true,bypassGateway=true`；
  - 空闲 5 分钟后对应 UDP flow 被回收（可用日志
    `UDP flow idle for 5m0s, closing` / `UDP ASSOCIATE idle for 5m0s, closing` 确认）。

## 部署记录（2026-11-20）

- 先灰度 QG：`v0.8.3-32-ge0303e2` upload+activate 成功；
- 随后全量扩散：9 节点全部 `v0.8.3-34-g312a646`、`status=succeeded`；
- QG 现状：`tun0` 在、fd 143、UDP socket 0，`/api/tun` 正常；拓扑 9 节点/22 条边；
- 待观察 fd/UDP 不随空闲流量增长（重点看 `ss -uanp | grep phaethon | wc -l`）；
- 建议 QG 的 OpenRC 启动前加 `ulimit -n 65535` 作为兜底（当前进程仍是 4095 软限制）。

