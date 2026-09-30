# gVisor 路由栈演进方向

## 状态：规划中

## 结论

**长期演进方向**：将 gVisor netstack 作为主路由栈，减少心智模型复杂度。

## 当前架构痛点

### 心智模型复杂
- 路由逻辑分散在多处：readLoop、writeLoop、mesh forward
- TTL 处理需要手动实现（递减、检查、ICMP 生成）
- 混杂模式绕过 gVisor 正常路由路径
- 调试时需要理解多层路由决策

### 代码重复
- readLoop 和 HandleMeshFrame 都需要处理 TTL
- writeLoop 需要重新分类数据包
- 路由判断逻辑在多个地方出现

## 演进目标架构

### 核心理念
所有路由决策集中在 gVisor 路由表，而不是分散在代码各处。

### 架构设计
```
readLoop (从 TUN 读取)
   ↓
全部注入 gVisor netstack
   ↓
gVisor 路由表决策
   ├─ TUN NIC → 回操作系统（本地交付）
   ├─ mesh NIC → mesh endpoint → P2P 链路
   └─ proxy NIC → proxy endpoint → 代理转发
   ↓
writeLoop (从 gVisor 读取)
   ↓
根据 NIC 类型发送到对应目标
```

### 关键组件

#### 1. 多个 NIC 定义
- **TUN NIC**：与操作系统交互，接收/发送本地数据包
- **mesh NIC**：mesh 网络接口，处理 P2P 路由、中继、failover
- **proxy NIC**：代理接口，处理 TCP/UDP 转发

#### 2. 动态路由表配置
根据 mesh 链路通告动态配置路由规则：
```go
// 示例：mesh 链路通告
{
  "destination": "100.179.0.0/16",
  "gateway": "100.179.0.1",  // GG 节点
  "metric": 1,
  "interface": "mesh0"
}

// gVisor 路由表自动更新
s.SetRouteTable([]tcpip.Route{
  {Destination: subnet, Gateway: gateway, NIC: meshNICID},
})
```

#### 3. 自定义 Endpoint
对于 mesh 特有的逻辑，需要自定义 endpoint：
- P2P 选路算法
- 中继逻辑
- Failover 机制
- 链路质量评估

### 收益

#### 心智模型简化
1. **TTL 自动处理**：gVisor 自动递减 TTL、生成 ICMP Time Exceeded
2. **路由集中化**：所有路由决策在 gVisor 路由表，不需要在代码多处判断
3. **标准 IP 行为**：符合标准网络协议，可以用标准工具调试
4. **减少代码重复**：不需要在 readLoop、writeLoop、mesh forward 重复路由逻辑

#### 可维护性提升
1. **调试更容易**：可以用 `ip route` 等工具查看路由表
2. **测试更简单**：路由逻辑由 gVisor 保证正确性
3. **扩展更容易**：新增路由规则只需要更新路由表

### 成本与风险

#### 实现成本
- **预估工期**：2-4 周
- **主要工作**：
  1. 定义多个 NIC 和 endpoint（1 周）
  2. 实现动态路由表配置（3-5 天）
  3. 实现 mesh endpoint（P2P 选路、中继、failover）（1 周）
  4. 迁移现有逻辑到 endpoint（3-5 天）
  5. 测试和调试（3-5 天）

#### 风险
1. **迁移风险**：现有系统稳定运行，迁移可能引入新问题
2. **性能风险**：gVisor 路由性能需要验证
3. **兼容性风险**：需要确保现有功能（代理、TUN、mesh）全部正常

### 迁移策略

#### 阶段 1：并行运行（1 周）
- 保留现有路由逻辑
- 新增 gVisor 路由表配置
- 双写模式：同时走新旧路径
- 对比结果，验证正确性

#### 阶段 2：逐步切换（2 周）
- 先切换简单场景（本地路由）
- 再切换 mesh 路由
- 最后切换代理转发
- 每个阶段充分测试

#### 阶段 3：清理旧代码（1 周）
- 删除旧的路由逻辑
- 更新文档
- 性能优化

## 当前优先级

**短期**：手动实现 traceroute（1-2 天）
- 在 readLoop 和 HandleMeshFrame 手动递减 TTL
- TTL=0 时手动生成 ICMP Time Exceeded
- 快速解决 traceroute 功能

**长期**：gVisor 路由栈演进（2-4 周）
- 作为架构优化项目推进
- 需要充分测试和验证
- 建议在功能稳定期实施

## 参考文档

- [gVisor ICMP TTL 调研](./gvisor_icmp_ttl_research.md)
- [Mesh Traceroute 设计](./mesh_traceroute_design.md)
