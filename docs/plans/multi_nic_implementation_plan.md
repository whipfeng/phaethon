# 多 NIC 架构实现计划

## 状态：❌ 已废弃 - 被 `gvisor_route_selector_architecture.md` 取代

**最终方案：RouteSelector + Link NICs（见 `gvisor_route_selector_architecture.md`）**

**关键变化**：
- ❌ **取消 Loopback NIC (NIC 3)**：IPIP 封装在 forwardUnicastPacket 中完成，不需要独立的 Loopback NIC
- ✅ **Link NICs**：每个直连 peer 一个 NIC（NIC 2, 3, 4...），而非共享 mesh NIC
- ✅ **RouteSelector**：在 FindRoute 过程中做动态路由决策，决定是否需要 IPIP 封装

**以下内容为历史实现记录，仅供参考，架构决策以 `gvisor_route_selector_architecture.md` 为准。**

---

## 目标

将当前单 NIC 架构重构为多 NIC 架构，充分利用 gVisor 的路由能力。

## 当前架构

```
单 NIC (channel.Endpoint, NIC 1)
  - 混杂模式 + spoofing
  - 所有流量共享
  - writeLoop 手动路由决策
  - IPIP 封装在 HandleOutboundPacket（TUN readLoop 拦截）
```

## 目标架构

```
NIC 1 (TUN): TUN 设备 I/O，NAT 边界
NIC 2 (Mesh): mesh 包接收/发送，绑定 GIP
NIC 3 (Loopback): IPIP 封装 + 环回
NIC 100+ (h_tunnel): 已存在，uplink-only
```

## 实现步骤

### 阶段 1：创建 LoopbackEndpoint（基础框架）✅ 已完成

**文件**：`mesh/loopback_endpoint.go`

**功能**：
- 实现 `stack.LinkEndpoint` 接口
- `WritePackets`：接收出站包，环回到入站
- 暂不实现 IPIP 封装（先验证基础框架）

**验证**：
- 创建 NIC 3，配置默认路由指向 NIC 3
- 验证包能正确环回

**实现细节**：
- 创建了完整的 `LoopbackEndpoint` 结构
- 实现了所有 `stack.LinkEndpoint` 接口方法
- 添加了统计信息收集

### 阶段 2：更新 Netstack 初始化 ✅ 已完成

**改动**：
- 在 `initStack` 中创建 NIC 3 (LoopbackEndpoint)
- 配置混杂模式
- 添加 `LoopbackEP()` getter 方法

**验证**：
- Netstack 初始化成功
- NIC 3 正确创建

**实现细节**：
- `netstack.go` 添加了 `loopbackEP` 字段
- `initStack` 创建 NIC 3 并配置
- 保持默认路由在 NIC 1（避免路由循环）

### 阶段 3：迁移 IPIP 封装到 NIC 3 ✅ 已完成

**改动**：
- 在 `LoopbackEndpoint.WritePackets` 中添加 IPIP 封装逻辑
- 实现 `extractDstIP`、`needsIPIPEncapsulation`、`encapsulatePacket`、`sendViaMesh` 方法
- 添加 `SetIPIPConfig` 方法配置 IPIP 参数
- 在 `MeshManager` 添加公共方法：`SelectEgressNodeIDForIP`、`SendEncapsulatedPacket`

**验证**：
- IPIP 封装逻辑正确
- 出口节点选择正确
- 代码编译通过

**实现细节**：
- 使用 `pkt.ToBuffer().Flatten()` 获取原始包数据
- 使用 `config.MeshStaticRoute` 匹配静态路由
- 调用 `meshMgr.SelectEgressNodeIDForIP` 选择出口节点和 EIP
- 调用 `tunnel.Encapsulate` 执行 IPIP 封装
- 调用 `meshMgr.SendEncapsulatedPacket` 发送封装后的包

**发现的路由循环问题**：
- 初步担心：如果将默认路由指向 NIC 3，LoopbackEndpoint 对非 IPIP 包调用 `dispatcher.DeliverNetworkPacket()` 后，可能会造成无限循环
- 实际测试：通过 gVisor 源码分析和测试验证，`DeliverNetworkPacket` 将包送回到入站路径后，会被混杂模式直接接收，**不会再次经过路由表查找**
- **结论**：不存在路由循环问题，可以安全地将默认路由指向 NIC 3

**当前状态**：
- ✅ 默认路由已改到 NIC 3
- ✅ IPIP 封装逻辑已实现并激活
- ✅ 代码编译通过

### 阶段 4：TUN NIC 独立（待实现）

**改动**：
- 将 channel.Endpoint 改为 TUN 设备 I/O
- 配置路由：VIP → NIC 1
- NAT 保持在 TUN 边界

**验证**：
- TUN 功能正常
- NAT 正确

### 阶段 5：QGT 环境验证（待实现）

**部署**：
- 在 QG 服务器上创建 QGT 环境
- 独立 subnet (100.65.0.0/16)
- 禁用 TUN（避免冲突）

**验证清单**：
- [ ] mesh 连通性
- [ ] 多 NIC 路由
- [ ] IPIP 封装
- [ ] h_tunnel 兼容性

**路由表配置计划**：
- ✅ 默认路由已改为指向 NIC 3（经测试验证不会造成路由循环）
- LoopbackEndpoint 会根据静态路由配置自动判断是否需要 IPIP 封装
- 匹配静态路由的包：IPIP 封装后通过 mesh 发送
- 不匹配的包：环回到入站路径，由 writeLoop 处理

## 风险和回滚

**风险**：
- 核心架构改动，可能引入新问题
- 需要充分测试
- 路由循环问题需要谨慎处理

**回滚**：
- 保留旧代码分支
- QGT 验证失败不影响 QG 生产
- 可以快速回滚到单 NIC 架构

## 时间估算

- 阶段 1：1-2 天 ✅ 实际 1 天
- 阶段 2：2-3 天 ✅ 实际 1 天
- 阶段 3：1-2 天 ✅ 实际 1 天
- 阶段 4：2-3 天（待实现）
- 阶段 5：1-2 天（待实现）
- **总计**：7-12 天，已完成 3-5 天

## 依赖

- gVisor netstack API 理解
- 现有 mesh 代码结构
- IPIP 封装逻辑（已实现）

## 下一步

1. 部署到 QGT 环境
2. 配置静态路由到 NIC 3
3. 测试 IPIP 封装功能
4. 验证 mesh 连通性
5. 完成阶段 4（TUN NIC 独立）
