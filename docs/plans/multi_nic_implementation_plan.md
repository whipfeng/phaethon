# 多 NIC 架构实现计划

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

### 阶段 1：创建 LoopbackEndpoint（基础框架）

**文件**：`mesh/loopback_endpoint.go`

**功能**：
- 实现 `stack.LinkEndpoint` 接口
- `WritePackets`：接收出站包，环回到入站
- 暂不实现 IPIP 封装（先验证基础框架）

**验证**：
- 创建 NIC 3，配置默认路由指向 NIC 3
- 验证包能正确环回

### 阶段 2：迁移 mesh 流量到 NIC 2

**改动**：
- 创建 mesh endpoint（或复用现有逻辑）
- 配置路由：mesh 网段 → NIC 2
- 修改 `InjectMeshPacket` 注入到 NIC 2
- 修改 mesh 发送逻辑，从 NIC 2 发出

**验证**：
- mesh 连通性正常
- 路由路径正确

### 阶段 3：迁移 IPIP 封装到 NIC 3

**改动**：
- 在 `LoopbackEndpoint.WritePackets` 中添加 IPIP 封装逻辑
- 移除 `HandleOutboundPacket` 中的 IPIP 拦截
- 配置静态路由匹配

**验证**：
- IPIP 封装正确
- 出口节点选择正确

### 阶段 4：TUN NIC 独立

**改动**：
- 将 channel.Endpoint 改为 TUN 设备 I/O
- 配置路由：VIP → NIC 1
- NAT 保持在 TUN 边界

**验证**：
- TUN 功能正常
- NAT 正确

### 阶段 5：QGT 环境验证

**部署**：
- 在 QG 服务器上创建 QGT 环境
- 独立 subnet (100.65.0.0/16)
- 禁用 TUN（避免冲突）

**验证清单**：
- [ ] mesh 连通性
- [ ] 多 NIC 路由
- [ ] IPIP 封装
- [ ] h_tunnel 兼容性

## 风险和回滚

**风险**：
- 核心架构改动，可能引入新问题
- 需要充分测试

**回滚**：
- 保留旧代码分支
- QGT 验证失败不影响 QG 生产
- 可以快速回滚到单 NIC 架构

## 时间估算

- 阶段 1：1-2 天
- 阶段 2：2-3 天
- 阶段 3：1-2 天
- 阶段 4：2-3 天
- 阶段 5：1-2 天
- **总计**：7-12 天

## 依赖

- gVisor netstack API 理解
- 现有 mesh 代码结构
- IPIP 封装逻辑（已实现）
