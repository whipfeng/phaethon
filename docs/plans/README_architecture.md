# gVisor 路由架构设计文档索引

## 最终方案（必读）

### 📄 [gvisor_route_selector_architecture.md](./gvisor_route_selector_architecture.md)

**状态**：✅ 最终方案（2026-10-04 定稿）

**核心内容**：
- RouteSelector 扩展点设计
- Link NICs 架构（每个直连 peer 一个 NIC）
- IPIP 封装在路由决策时完成（无 Tunnel NIC）
- 完整流量路径示例（具体 IP）
- VIP/EIP 从子网算出（无映射表）
- 路由缓存兼容性分析

**必读场景**：
- 理解最终架构
- 实施阶段 2（fork 定制）
- 调试路由/NAT/IPIP 问题

---

## 历史文档（仅供参考）

### 📄 [gvisor_stack_integration_design.md](./gvisor_stack_integration_design.md)

**状态**：⚠️ 部分过时（被 `gvisor_route_selector_architecture.md` 更新）

**仍然有效**：
- gVisor 扩展点盘点（NAT、IPIP 解封装、混杂模式）
- 源码调研详情
- 讨论脉络纪要

**已过时**：
- Tunnel NIC 设计 → 改为路由时封装
- NIC 规划（隧道 NIC 201+）→ 改为 Link NICs
- 数据流（隧道 NIC 相关）→ 改为转发路径封装

---

### 📄 [gvisor_routing_evolution.md](./gvisor_routing_evolution.md)

**状态**：❌ 已废弃

**历史价值**：
- gVisor NAT 能力边界调研
- 2026-10-03 勘误（DNAT 死结证伪）
- 早期多 NIC 架构探索

---

### 📄 [multi_nic_architecture.md](./multi_nic_architecture.md)

**状态**：❌ 已废弃（v1）

**历史价值**：
- 早期多 NIC 架构设计
- NIC 职责划分探索

---

### 📄 [multi_nic_architecture_v2.md](./multi_nic_architecture_v2.md)

**状态**：❌ 已废弃（v2，手动 NAT 方案）

**历史价值**：
- 手动 NAT 实现细节
- NIC 3/4 统一分发逻辑
- Spoofing 机制调研

---

### 📄 [traffic_flow_analysis.md](./traffic_flow_analysis.md)

**状态**：✅ 已整合到 `gvisor_route_selector_architecture.md`

**历史价值**：
- 流量路径分析（已整合到新文档 §4）
- 核心冲突分析（默认路由方向、fakeIP 处理）

---

### 📄 [mesh_ipip_smart_routing.md](./mesh_ipip_smart_routing.md)

**状态**：✅ 仍然有效

**内容**：
- IPIP 出口节点选择算法（静态优先、sticky、hash 稳定）
- 链路质量监控（Phase 2）
- 智能选路（Phase 3）

**注意**：
- Phase 1（IPIP 封装）的实现位置已更新（在转发路径，不在 Tunnel NIC）
- 选路算法仍然有效

---

## 文档演进时间线

```
2026-09-17  gvisor_routing_evolution.md
            └─ gVisor NAT 能力调研
            └─ 结论：NAT 必须在 TUN 边界（后证伪）

2026-10-01  multi_nic_architecture.md (v1)
            └─ 早期多 NIC 架构设计

2026-10-02  multi_nic_architecture_v2.md (v2)
            └─ 手动 NAT 方案
            └─ NIC 3/4 统一分发

2026-10-03  gvisor_routing_evolution.md 勘误
            └─ 证伪：DNAT 死结不成立
            └─ 结论：栈内 NAT 可行

2026-10-03  gvisor_stack_integration_design.md
            └─ gVisor fork 定制方案
            └─ Tunnel NIC + proto=4 处理器

2026-10-04  gvisor_route_selector_architecture.md ⭐ 最终方案
            └─ 取消 Tunnel NIC
            └─ RouteSelector 扩展点
            └─ Link NICs 设计
            └─ IPIP 封装在路由决策时完成
```

---

## 快速导航

| 我想... | 阅读文档 |
|---------|---------|
| 理解最终架构 | `gvisor_route_selector_architecture.md` |
| 了解 gVisor 扩展点 | `gvisor_stack_integration_design.md` §2 |
| 看流量路径示例 | `gvisor_route_selector_architecture.md` §4 |
| 理解 IPIP 选路算法 | `mesh_ipip_smart_routing.md` |
| 了解历史演进 | 本文档"时间线"章节 |
| 调试 NAT 问题 | `gvisor_stack_integration_design.md` §3.1 |
| 调试 IPIP 问题 | `gvisor_route_selector_architecture.md` §3 |
