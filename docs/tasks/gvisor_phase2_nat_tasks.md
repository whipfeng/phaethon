# gVisor 阶段 2：NAT 完整解决方案实施任务

## 概述

实现 gVisor fork 补丁 #3-#6，完成 NAT 完整解决方案，支持旁路网关任意源 IP 回程。

## 任务清单

### 任务 1：Fork 补丁 #3 - RouteSelector 扩展点

**文件**：`../gvisor-fork/pkg/tcpip/stack/stack.go`

**目标**：在 FindRoute 中注入自定义路由决策逻辑

**实现**：
```go
// 新增 RouteSelector 类型
type RouteSelector func(dst tcpip.Address) RouteDecision

type RouteDecision struct {
    NeedIPIP      bool
    EgressVIP     tcpip.Address
    Cacheable     bool
    LocalDelivery bool
}

// Stack 新增字段
type Stack struct {
    // ...
    routeSelector RouteSelector
}

// FindRoute 中调用
if s.routeSelector != nil {
    decision := s.routeSelector(remoteAddr)
    if decision.LocalDelivery {
        return makeLocalRoute(...)
    }
    if decision.NeedIPIP {
        return makeIPIPRoute(decision.EgressVIP)
    }
}
```

**验收**：
- [ ] RouteSelector 可以被设置
- [ ] FindRoute 调用 RouteSelector
- [ ] 编译通过

---

### 任务 2：Fork 补丁 #4 - Postrouting InputInterface 匹配

**文件**：`../gvisor-fork/pkg/tcpip/stack/iptables_types.go`

**目标**：Postrouting hook 支持 InputInterface 匹配

**实现**：
```go
// iptables_types.go:320-321
func (r *Rule) checkPostrouting(pkt *Packet, ...) bool {
    // 现状：return true
    // 改为：
    if r.InputInterface != "" {
        return pkt.InputInterfaceName == r.InputInterface
    }
    return true
}
```

**验收**：
- [ ] Postrouting 规则可以设置 InputInterface
- [ ] 匹配逻辑正确
- [ ] 编译通过

---

### 任务 3：Fork 补丁 #5 - Conntrack 记录输入接口

**文件**：`../gvisor-fork/pkg/tcpip/stack/conntrack.go`

**目标**：Conntrack 条目记录原始输入接口

**实现**：
```go
type conntrackEntry struct {
    // ... 现有字段
    OriginalInputNIC tcpip.NICID  // 新增
}

// 创建条目时记录
func (ct *conntrack) createEntry(pkt *PacketBuffer, ...) *conntrackEntry {
    entry := &conntrackEntry{
        OriginalInputNIC: pkt.NICID,
    }
    return entry
}

// 查询方法
func (ct *conntrack) GetOriginalInputNIC(pkt *PacketBuffer) tcpip.NICID {
    entry := ct.lookup(pkt)
    if entry != nil {
        return entry.OriginalInputNIC
    }
    return 0
}
```

**验收**：
- [ ] Conntrack 条目包含 OriginalInputNIC
- [ ] 创建时正确记录
- [ ] 查询方法可用
- [ ] 编译通过

---

### 任务 4：Fork 补丁 #6 - DNAT 辅助路由

**文件**：`../gvisor-fork/pkg/tcpip/network/ipv4/ipv4.go` + `stack.go`

**目标**：DNAT 回程包自动路由到原始输入接口

**实现**：
```go
// ipv4.go Prerouting 后
if pkt.NATType == DNAT {
    if entry := stack.conntrack.LookupByDestination(pkt.DstAddr); entry != nil {
        pkt.OutputNIC = entry.OriginalInputNIC
    }
}

// stack.go FindRoute
func (s *Stack) FindRoute(...) (*Route, error) {
    if pkt.OutputNIC != 0 {
        return makeRoute(pkt.OutputNIC, ...)
    }
    // 正常路由
}
```

**验收**：
- [ ] DNAT 后 pkt.OutputNIC 被设置
- [ ] FindRoute 使用 pkt.OutputNIC
- [ ] 回程包正确路由
- [ ] 编译通过

---

### 任务 5：实现 Link NICs 架构

**文件**：`mesh/netstack.go`

**目标**：每个直连 mesh peer 一个 NIC

**实现**：
```go
// 为每个直连 peer 创建 Link NIC
for _, peer := range directPeers {
    nicID := tcpip.NICID(2 + peerIndex)
    s.CreateNIC(nicID, NewLinkEndpoint(peer))
    // 添加路由：peer subnet → Link NIC
}
```

**验收**：
- [ ] 每个直连 peer 有独立 NIC
- [ ] 路由表正确
- [ ] 编译通过

---

### 任务 6：IPIP 封装内化

**文件**：`mesh/netstack.go` + gVisor fork

**目标**：IPIP 封装在转发路径完成（无 Tunnel NIC）

**实现**：
```go
// RouteSelector 决策
func routeSelector(dst tcpip.Address) RouteDecision {
    if !isMeshSubnet(dst) {
        egressVIP := selectEgressNode(dst)
        return RouteDecision{
            NeedIPIP: true,
            EgressVIP: egressVIP,
        }
    }
    return RouteDecision{}
}

// 转发代码处理 NeedIPIP
if route.NeedIPIP {
    encapsulateIPIP(pkt, route.EgressVIP)
    // 重新路由外层包
}
```

**验收**：
- [ ] 非 mesh 目标触发 IPIP 封装
- [ ] 封装后正确路由
- [ ] 编译通过

---

### 任务 7：配置 NAT 规则

**文件**：`mesh/netstack.go`

**目标**：配置 SNAT + DNAT + conntrack

**实现**：
```go
// SNAT 规则（补丁 #4）
natTable.AddRule(iptables.Rule{
    InputInterface: "NIC1",
    Target: &iptables.SNATTarget{
        Addresses: []tcpip.Address{vipAddr},
    },
})

// DNAT 由 conntrack 自动处理
// 补丁 #5+#6 实现辅助路由
```

**验收**：
- [ ] SNAT 只对 NIC1 进入的流量生效
- [ ] DNAT 回程正确
- [ ] 编译通过

---

### 任务 8：拦截器退役

**文件**：`tun/engine.go`, `mesh/mesh.go`

**目标**：移除旧的 mesh 拦截器

**实现**：
- 移除 `meshOutboundCh` 拦截逻辑
- 移除 `HandleOutboundPacket` 方法
- 所有 mesh 收帧直接 InjectMeshPacket

**验收**：
- [ ] 拦截器代码移除
- [ ] mesh 流量正常
- [ ] 编译通过

---

### 任务 9：VM 环境部署测试

**目标**：在 VM 环境验证完整功能

**测试项**：
- [ ] 旁路网关 NAT（域名、raw IP）
- [ ] mesh 路由（直连、非直连）
- [ ] IPIP 封装/解封装
- [ ] DNAT 回程（任意源 IP）
- [ ] 长连接 TCP（无 RST 循环）
- [ ] UDP/QUIC

**部署步骤**：
1. 编译 Windows 版本
2. 上传到 VM 环境
3. 停止旧进程
4. 启动新进程
5. 运行测试

---

## 实施顺序

1. 任务 1-4：Fork 补丁（按顺序）
2. 任务 5-6：架构实现
3. 任务 7：NAT 配置
4. 任务 8：清理旧代码
5. 任务 9：部署测试

## 预估工期

- Fork 补丁：2-3 天
- 架构实现：2-3 天
- 测试验证：1-2 天
- **总计：5-8 天**

## 风险

1. **Fork 补丁复杂度**：conntrack 修改可能影响现有 NAT 逻辑
2. **Link NICs 性能**：多个 NIC 可能增加开销
3. **DNAT 辅助路由**：需要确保 conntrack 查询性能

## 回退方案

每个任务独立可回退，如果某个补丁有问题可以单独回滚。
