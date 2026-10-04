# gVisor RouteSelector + Link NICs 实现任务

## 任务信息

- 分支名：`gvisor-route-selector-impl`
- 目标：实现 gVisor RouteSelector 扩展点集成 + Link NICs 架构，完成阶段 2 架构升级
- 创建日期：2026-10-04
- 依赖计划：[gvisor_route_selector_architecture.md](../plans/gvisor_route_selector_architecture.md)

## 背景

### 当前实现状态

**已完成（阶段 1）**：
- ✅ 2-NIC 拓扑（NIC 1: TUN, NIC 2: Mesh）
- ✅ NIC 1 混杂模式 + spoofing
- ✅ 删除手动 NAT（nat.go 已删）
- ✅ Fork gVisor + go.mod replace
- ✅ gVisor fork 补丁 #1-#6 已实现并推送

**未实现（阶段 2 缺口）**：
- ❌ RouteSelector 未在 phaethon 中集成（SetRouteSelector 未调用）
- ❌ Link NICs 架构未实现（当前仍用 Tunnel NICs，来自旧设计）
- ❌ IPIP 封装未在转发路径（forwardUnicastPacket）完成

### 设计文档要求

根据 `gvisor_route_selector_architecture.md` 最终方案：

1. **NIC 规划**：
   - NIC 1: TUN（fakeIP 子网 / 宿主网段）
   - NIC 2+: Link NICs（每个直连 mesh peer 一个）
   - **无 Tunnel NIC**（IPIP 封装在路由决策时完成）

2. **RouteSelector 扩展点**：
   - 在 FindRoute 过程中注入自定义路由决策
   - 处理 fakeIP 本地交付、IPIP 封装决策
   - VIP/EIP 从子网算出（VIP=subnet+1, EIP=subnet+4）

3. **IPIP 封装路径**：
   - 在 forwardUnicastPacket 完成（不是 Tunnel NIC）
   - 封装后外层包重新 FindRoute，匹配 Link NIC

## 阶段 1: 集成 RouteSelector

### Task 1.1: 在 initStack 中调用 SetRouteSelector

**文件**：`mesh/netstack.go`

**改动**：
1. 创建 RouteSelector 函数实现
2. 在 initStack 中调用 `s.SetRouteSelector(routeSelector)`
3. RouteSelector 逻辑：
   - fakeIP → LocalDelivery=true
   - mesh 子网 → 空决策（路由表处理）
   - 非 mesh 目标 → NeedIPIP=true + EgressVIP

**验证**：
- [ ] 代码编译通过
- [ ] RouteSelector 被正确设置
- [ ] 日志显示 RouteSelector 已激活

### Task 1.2: 实现 VIP/EIP 计算逻辑

**文件**：`mesh/netstack.go` 或新建 `mesh/route_selector.go`

**改动**：
1. 实现 `calculateVIP(subnet net.IPNet) net.IP`
2. 实现 `calculateEIP(subnet net.IPNet) net.IP`
3. RouteSelector 使用这些函数计算 EgressVIP

**验证**：
- [ ] VIP/EIP 计算正确（subnet+1, subnet+4）
- [ ] 与 mesh 节点的 subnet 配置一致

## 阶段 2: 实现 Link NICs 架构

### Task 2.1: 创建 LinkNIC 类型

**新建文件**：`mesh/link_nic.go`

**改动**：
1. 实现 `LinkEndpoint` 接口
2. WritePackets：直接发送给直连 peer（交给 mesh hop 表选路）
3. 不做 IPIP 封装（封装在 forwardUnicastPacket）
4. 不做路由决策（路由在 FindRoute 完成）

**验证**：
- [ ] LinkNIC 实现所有 LinkEndpoint 方法
- [ ] WritePackets 正确调用 mesh 发送接口

### Task 2.2: 替换 Tunnel NICs 为 Link NICs

**文件**：`mesh/netstack.go`

**改动**：
1. 移除 TunnelNIC 创建逻辑
2. 为每个直连 peer 创建 LinkNIC
3. 配置路由表：peer 子网 → Link NIC
4. 更新 UpdateTunnelNICs 为 UpdateLinkNICs

**验证**：
- [ ] 每个直连 peer 对应一个 LinkNIC
- [ ] 路由表正确配置
- [ ] 编译通过

### Task 2.3: 同步 mesh 路由到 gVisor 路由表

**文件**：`mesh/netstack.go`

**改动**：
1. 在 UpdateLinkNICs 中同步通告路由
2. 调用 SetRouteTable 更新路由表
3. 确保直连 peer 子网有明确路由

**验证**：
- [ ] 路由表包含所有直连 peer 子网
- [ ] 路由指向对应的 LinkNIC

## 阶段 3: IPIP 封装内化

### Task 3.1: 在 forwardUnicastPacket 实现 IPIP 封装

**文件**：gVisor fork `pkg/tcpip/network/ipv4/ipv4.go` + `ipv6.go`

**改动**：
1. 检查 Route.NeedIPIP 标志
2. 如果需要 IPIP：
   - 构造外层包（src=EIP, dst=EgressVIP, proto=4）
   - 调用 phaethon 的 IPIP 封装函数
3. 封装后重新 FindRoute（外层包路由）

**依赖**：
- 需要 phaethon 提供 IPIP 封装接口
- 需要 RouteSelector 返回 NeedIPIP + EgressVIP

**验证**：
- [ ] IPIP 封装在转发路径完成
- [ ] 外层包正确路由到 LinkNIC
- [ ] 编译通过

### Task 3.2: 移除 TunnelNIC 的 IPIP 封装逻辑

**文件**：`mesh/tunnel_nic.go`（删除）

**改动**：
1. 删除 TunnelNIC 类型
2. 删除 IPIP 封装相关代码
3. 确保没有其他地方引用 TunnelNIC

**验证**：
- [ ] TunnelNIC 完全移除
- [ ] 编译通过
- [ ] 无未使用代码

## 阶段 4: 验证与测试

### Task 4.1: 本地编译验证

**验证**：
- [ ] `go build ./...` 成功
- [ ] `go test ./...` 无回归
- [ ] 无编译警告

### Task 4.2: VM 环境部署验证

**部署**：
- [ ] 编译 Windows 版本
- [ ] 部署到 VM 环境
- [ ] 验证启动成功

**功能验证**：
- [ ] mesh 连通性（ping 其他节点）
- [ ] 旁路网关 NAT（域名访问）
- [ ] 旁路网关 NAT（raw IP 访问）
- [ ] IPIP 封装/解封装
- [ ] 长连接 TCP（SSH over mesh）
- [ ] UDP/QUIC

### Task 4.3: QG 环境部署验证

**部署**：
- [ ] 编译 Linux 版本
- [ ] 部署到 QG 环境
- [ ] 验证服务正常

**功能验证**：
- [ ] 旁路网关 NAT（LAN 机器访问外网）
- [ ] mesh 路由（节点间通信）
- [ ] DNS 解析（fakeIP）
- [ ] 回程流量（DNAT）

### Task 4.4: 更新文档并收尾

**更新**：
- [ ] 更新 `docs/plans/gvisor_route_selector_architecture.md` 实现状态
- [ ] 更新 `docs/index.md`（如需要）
- [ ] 标记所有 task 为 [x]
- [ ] commit 并 push

## 实施顺序

1. **阶段 1**：集成 RouteSelector（Task 1.1-1.2）
2. **阶段 2**：实现 Link NICs（Task 2.1-2.3）
3. **阶段 3**：IPIP 封装内化（Task 3.1-3.2）
4. **阶段 4**：验证与测试（Task 4.1-4.4）

## 风险与回滚

**风险**：
- 核心架构改动，可能引入新问题
- gVisor fork 修改需要重新编译
- 路由语义变化可能影响现有功能

**回滚**：
- 保留旧代码分支（master）
- 可以回退到 2-NIC + Tunnel NIC 架构
- gVisor fork 可以回退到无补丁版本

## 依赖

- gVisor fork 补丁 #1-#6（已完成）
- RouteSelector 接口定义（gVisor fork）
- mesh 路由表 API（GetRouteTable, SelectEgressNodeIDForIP）
- IPIP 封装函数（tunnel.Encapsulate）

## 时间估算

- 阶段 1：1-2 天
- 阶段 2：2-3 天
- 阶段 3：2-3 天
- 阶段 4：2-3 天
- **总计**：7-11 天
