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

1. **主因：handler 的 UDP relay 没有空闲超时**（`handler/connection_handler.go`）。
   - QG 作为旁路网关承载 LAN 的 UDP（DNS/QUIC 等）；每个 direct UDP flow 创建
     `targetConn = dialer.ListenPacketBoundTo("udp", ...)`；
   - `relayUDP` 只在 netstack 连接关闭/读写错误时关闭 `targetConn`，自身没有空闲超时；
   - gVisor 的 UDP association 对空闲 flow 不会主动结束，于是每条空闲 UDP flow
     永久占着一个 OS UDP socket；
   - 与现场 “4052 个无连接 UDP、无对应 TCP” 完全吻合。
2. **次因：SOCKS5 UDP ASSOCIATE 也没有空闲超时**（`server/socks5.go`）。
   - 控制 TCP 活着但长期无 UDP 时，relay 不会关闭；虽然用户当前主要用旁路网关，
     该路径仍是潜在泄漏点。
3. fd 上限 4095 偏低：泄漏累积到上限后，TUN 创建/重建、DNS、admin TLS 全部失败，
   表现为“TUN 不可用/旁路网关失效”，并非进程崩溃。

## 修复

- `relayUDP`：增加 5 分钟空闲 watchdog（每 30s 检查），空闲后关闭
  netstack 连接和 targetConn；watchdog 不依赖 `SetReadDeadline`，通过关闭连接唤醒读循环。
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
