# RouteFunc 指定链路 gvisor socket：路由构造与 IPIP 始发封装修复

日期：2026-11-02
状态：方案已对齐，实施中
关联：docs/issues/mesh-data-plane-stall.md（取证背景）、gvisor_route_selector_architecture.md

## 背景与根因（已实锤闭合）

从 QG 走 WIN7_VPN/qgw 指定链路访问 10.181.58.10:19090 不通，证据链：

1. forwarder 匹配规则 → NetDialV2 → RouteFunc 返回正确决策
   `{EgressNIC=linkNIC, EgressEIP=100.209.0.4, NeedIPIP=true}`（[V2-ROUTE] 日志）。
2. `BuildRouteFromDecision`（stack.go）localAddr/remoteAddr 全留空 → LinkNIC 无绑定地址
   → `getAddressEP` 返回 nil → `Route.WritePacket` 在 `isValidForOutgoing` 返回
   `ErrInvalidEndpointState`。
3. `sendSynTCP`（connect.go）吞掉该错误（"We ignore SYN send errors"）→ SYN 从未发出，
   connect 100s 超时，qgw 全程零感知。
4. 第二层缺陷：IPIP 封装只存在于转发路径 `forwardWithIPIPEncapsulation`；
   endpoint 始发路径 `writePacketPostRouting` 无 NeedIPIP 分支。即使路由修好，
   裸 IP 目标（10.181.58.10）的包也会从 linkNIC 直接裸发，qgw 无法解封。
   域名/fakeIP 规则走转发路径（有封装）所以今晨 403 能通——双路径不对称是病根。

## 修复方案（5 处）

### Task 1：BuildRouteFromDecision 补地址（gvisor-fork stack.go）

签名改为：

```go
func (s *Stack) BuildRouteFromDecision(decision RouteDecision, netProto tcpip.NetworkProtocolNumber,
    localAddr, remoteAddr tcpip.Address) (*Route, tcpip.Error)
```

- localAddr 属主 NIC：遍历 `s.nics`，`nic.hasAddress(netProto, localAddr)` 命中即为
  localAddressNIC（GIP 绑 NIC 1，LinkNIC 是 outgoingNIC=decision.EgressNIC，
  跨 NIC 路由模型，与 `findRouteWithLocalAddrFromAnyInterfaceRLocked` 同构）。
- `getAddressEP(localAddressNIC, localAddr, ...)` 取端点；nil 则返回
  `ErrBadLocalAddress`（响亮失败，不再构造无效路由）。
- IPIP / 非 IPIP 合并为单条 makeRoute 调用，needIPIP / egressEIP 取自 decision。
- 调用点（tcp endpoint.go connect）：传 `e.TransportEndpointInfo.ID.LocalAddress`
  （NetDialV2 已 bind GIP）与 `addr.Addr`（真实目标）。

### Task 2：ID.LocalAddress 覆盖加通配守卫（gvisor-fork tcp endpoint.go）

现状 `e.TransportEndpointInfo.ID.LocalAddress = r.LocalAddress()` 无条件覆盖，
RouteFunc 路径下曾把 bind 好的 GIP 抹空（路由空地址时）。干净方案：恢复覆盖行的
原始语义——仅当 bind 地址未指定（wildcard）时才采纳路由解析出的地址；显式 bind
的地址永远优先：

```go
// Only adopt the route's local address when the endpoint was not bound to a
// specific one; an explicit bind always wins.
if la := e.TransportEndpointInfo.ID.LocalAddress; la.BitLen() == 0 || la == header.IPv4Any || la == header.IPv6Any {
    e.TransportEndpointInfo.ID.LocalAddress = r.LocalAddress()
}
```

### Task 3：始发路径补 NeedIPIP 封装（gvisor-fork ipv4.go）

`writePacketPostRouting` 在 CheckPostrouting 通过后、MTU 计算前插入：

```go
if r.NeedIPIP && r.EgressEIP != (tcpip.Address{}) {
    return e.writeWithIPIPEncapsulation(r, pkt)
}
```

新函数 `writeWithIPIPEncapsulation(r, pkt) tcpip.Error`：

- 内层包此路径已在本函数前做过 CheckPostrouting（SNAT 语义与转发路径一致），
  直接 `pkt.ToView()` 取全包；
- 外层构造抽公共函数 `newIPIPOuterPacket(localEIP, egressEIP, innerPkt)` 与
  `forwardWithIPIPEncapsulation` 共用（src=本机 EIP 回退 route.LocalAddress()，
  dst=route.EgressEIP，proto=4，TTL=64）；
- `FindRoute(0, {}, egressEIP, ...)` 走外层路由，再
  `forwardPacketWithRoute(outerRoute, outerPkt, false)`——与转发路径完全同路。
- `ip.ForwardingError` 不是 `tcpip.Error`：nil 直接返回；`*ip.ErrOther` 解包返回
  内层错误；其余映射 `ErrHostUnreachable`。

### Task 4：sendSynTCP 错误不静默（gvisor-fork tcp connect.go）

保留"忽略错误、靠重试"的语义不变，仅加错误日志：

```go
if err := e.sendTCP(r, tf, p, stack.GSO{}); err != nil {
    e.stats.SendErrors.SynSendToNetworkFailed.Increment()
    util.LogModuleError("gvisor", "[TCP-SYN-SEND] failed: remote=%s:%d local=%s:%d err=%v",
        r.RemoteAddress(), tf.dstPort, r.LocalAddress(), tf.srcPort, err)
}
```

### Task 5：清理（phaethon mesh/netstack.go）

- 删除零调用者死代码 `AddMeshVIP`（:820-838）；VIP 运行时不绑任何 NIC。
- 更正 initStack 过时注释（:710"VIP is bound via AddMeshVIP"→如实描述：
  VIP 仅为 NAT/SNAT 身份，不绑 NIC；EIP 为隧道外层身份）。

## 不做的事

- 不改 NetDialV2 / RouteFunc 决策逻辑（决策本身正确）。
- 不改转发路径封装行为（今晨 403 已验证可用）。
- 不加任何兼容 shim / 旧行为 fallback。

## 验证

### 本地

- `go build ./...`（gvisor-fork 与 phaethon 均过）。
- `go test ./mesh/...` 及受影响的 gvisor-fork 包编译。

### VM 部署验证（部署顺序 VM → 用户确认 → QG；QGW 只放二进制不重启）

前提：VM worker 处于留跑健康巡检取证期，重启时机须与用户确认。

1. 版本戳构建部署 VM（-a -ldflags git describe）。
2. 复现：`curl http://10.181.58.10:19090/` 经 WIN7_VPN/qgw 规则。
3. VM 日志观测点：
   - `[V2-ROUTE] proxy=WIN7_VPN linkNICID=10 ... eip=100.209.0.4`
   - `ipip outer_header: src=<本机EIP> dst=100.209.0.4 proto=4`
   - `forwardPacketWithRoute OK`（SYN 真正离栈）
4. qgw 日志观测点：解封后 forward 记录（inner dst=GIP/目标）。
5. 页面真实返回（非 100s 超时）。
6. 回归：域名/fakeIP 规则路径（转发路径封装）仍通。

## 风险

- FindRoute(0,{},egressEIP) 外层路由依赖 RouteSelector 对 mesh 地址返回空决策
  后走 mesh 链路路由——与转发路径完全一致，风险相同（今晨 403 已验证）。
- sendSynTCP 日志在重试场景会重复打（可接受，LogModuleError 级别）。
