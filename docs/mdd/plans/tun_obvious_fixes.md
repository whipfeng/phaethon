# TUN 显而易见问题修复计划

## 元数据

- 文档类型：Plan
- 版本：v0.1.0
- 所属项目：phaethon
- 创建日期：2026-08-25

## 背景

` tun/engine.go`、`tun/device_windows.go` 等文件已实现基于 gvisor netstack 的 TUN 流量拦截。代码 review 中发现几个不需要大改架构、但容易出问题的明显缺陷：

1. `readLoop` 把所有包都按 IPv4 注入 netstack，IPv6 包会被错误解析。
2. `readLoop` 复用同一个 2048 字节 buffer，存在数据被覆盖的风险。
3. `proxyDesc` 对 DIRECT 的判断因为大小写不一致而失效。

本次修复只针对这 3 个显而易见的问题，**不开启真实 TUN 进行整机测试**（避免把当前工作网络搞挂）。真实 TUN 拦截测试留到后续在独立 VM/测试机上进行。

## 新增目标

5. 排除 LAN/私网流量，避免 TUN 启用后本地网络中断。
6. 修复 UDP 转发语义，使用 `net.PacketConn` 保持数据报边界，避免把 UDP 当 TCP 流 relay。

## 阶段 1: readLoop 修复

### Task 1.1: IPv4/IPv6 协议识别
- 读取 TUN 包后，根据 IP 头第一个字节的高 4 位判断版本：
  - `0x40` (4) → IPv4
  - `0x60` (6) → IPv6
- 分别调用 `linkEP.InjectInbound(ipv4.ProtocolNumber, pkt)` 或 `linkEP.InjectInbound(ipv6.ProtocolNumber, pkt)`。
- 非 IPv4/IPv6 包直接丢弃。

### Task 1.2: 每包独立 buffer
- 在 `readLoop` 里每次读到数据后，复制到新的 `make([]byte, n)` 再交给 `buffer.MakeWithData`。
- 替代当前复用 `buf := make([]byte, 2048)` 的方案。
- 为减少 GC 压力，可引入 `sync.Pool`（可选，本次先保证正确性）。

## 阶段 2: proxyDesc 大小写修复

### Task 2.1: 统一大小写比较
- `proxyDesc` 中使用 `strings.EqualFold(p.Type, config.ProxyDIRECT)` 替代 `p.Type == config.ProxyDIRECT`。
- 因为配置初始化时 `proxy.Type = strings.ToLower(proxy.Type)`，直接等于大写常量会永远失败。

## 阶段 3: LAN/私网排除

### Task 3.1: 默认 LAN CIDR 排除
- 在 `tun/route.go` 新增 `DefaultLANExclusions`，包含常见 IPv4 私网/本地/组播前缀：
  - `10.0.0.0/8`
  - `172.16.0.0/12`
  - `192.168.0.0/16`
  - `127.0.0.0/8`
  - `169.254.0.0/16`
  - `224.0.0.0/4`
  - `255.255.255.255/32`
- `Engine.Start()` 将 LAN CIDR 与代理服务器 IP 合并后传给 `RouteManager.SetExclusions`。

### Task 3.2: RouteManager 支持 CIDR 排除
- `platformSetup` 遍历 exclusions 时同时处理纯 IP 和 CIDR：
  - Windows: 通过 `parseExclusionCIDR` 区分 `/32` 与具体前缀长度。
  - Linux: 通过 `parseExclusionLinux` 生成 `*net.IPNet`。
  - Darwin: `route -n add -host` / `route -n add -net` 分别处理。
- `deleteExclusionRoute` 同样根据是否含 `/` 删除主机或网络路由。

## 阶段 4: UDP 报文转发

### Task 4.1: 使用 net.PacketConn 保持数据报语义
- `handleUDP` 不再调用 `dialer.ChainDialWithID` 把 UDP 伪装成 TCP。
- 代理流量改用 `dialer.ChainUDPDial(proxy)` 获取 `net.PacketConn`。
- DIRECT UDP 通过 `directDialPacket()` 创建绑定原物理网卡的 UDP socket，绕过 TUN 路由。
- 新增 `relayUDP()`：
  - 一个方向从 `netstackConn.Read` 读取一个 UDP 数据报，通过 `targetConn.WriteTo` 发给目标。
  - 另一个方向从 `targetConn.ReadFrom` 读取一个数据报，通过 `netstackConn.Write` 写回 netstack。
  - 这样可以保持 UDP 数据报边界，不混淆多个数据包。

## 阶段 5: 验证

### Task 5.1: 单元测试
- `go test ./tun -v` 必须全部通过。
- `go test ./...` 无回归。

### Task 5.2: 代码静态检查
- `go build ./...` 成功。
- `go vet ./tun` 无新增告警。

## 验收标准

- `readLoop` 正确处理 IPv4/IPv6。
- `readLoop` 不再复用单 buffer。
- `proxyDesc` 对 DIRECT 显示为 `"DIRECT"`。
- LAN/私网流量默认绕过 TUN。
- UDP 转发保持数据报语义，不再当 TCP 流处理。
- `go test ./...` 通过，`go build ./...` 成功。
- 不在当前工作主机上启用真实 TUN。
