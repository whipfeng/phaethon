# Mesh IPIP 隧道 + 管理面板优化设计

## 概述

本设计文档涵盖三个任务：
1. **Mesh IPIP 隧道**：在 mesh 层实现 IP-in-IP 封装，支持策略路由指定出口节点
2. **管理面板优化**：将日志和活跃连接从仪表盘移到独立页面
3. **SetReadDeadline 调研**：验证 gVisor 的 SetReadDeadline 是否可用，watchdog 是否必要

---

## 任务一：Mesh IPIP 隧道

### 背景与动机

当前 mesh 路由依赖节点通告的路由表。但某些场景需要：
- **策略路由**：在入口节点声明规则，指定流量从哪个出口节点发出
- **免通告路由**：不依赖通告的路由表，直接通过外层 IPIP 头路由
- **流量工程**：负载均衡、故障切换、地理路由等

### 设计目标

1. 在 mesh 层实现 IPIP 封装/解封装
2. 支持规则匹配指定出口节点
3. 利用外层 IPIP 头的路由能力
4. 与现有 TUN/mesh 架构解耦

### 架构设计

#### IP 地址规划

- **VIP**：100.x.x.x（现有，用于 NAT/内部寻址）
- **EIP**：从节点现有网段中预留专用 IP（如 100.0.0.254）
  - 不需要额外通告
  - 每个节点从自己的子网中分配
  - 用于 IPIP 外层头

#### 数据流

**去程（封装）**：
```
本地应用 → TUN 设备 → gVisor netstack → NAT（src=VIP/GIP）
  ↓
规则匹配："dst=X via node Y"
  ↓
Mesh 层封装：
  - 外层 IP 头：src=本地 EIP, dst=出口节点 EIP, protocol=4 (IPIP)
  - 内层 IP 包：原始包（src=NAT'd VIP/GIP, dst=X）
  ↓
Mesh P2P 发送到出口节点
```

**回程（无需特殊处理）**：
```
目标服务器响应 → 回程包（src=X, dst=NAT'd VIP/GIP）
  ↓
出口节点网络栈 → 正常路由回本地节点
```

**出口节点（解封装）**：
```
Mesh P2P 收到 IPIP 包（protocol=4）
  ↓
检查外层 IP 头
  ↓
剥离外层头，暴露内层 IP 包
  ↓
注入本地 gVisor netstack（或 TUN 设备）
  ↓
内层包继续路由到最终目标
```

#### 每层 IP 包源地址

| 层级 | 源地址 | 目标地址 | 说明 |
|------|--------|----------|------|
| 内层包 | NAT'd VIP/GIP | 最终目标 | TUN 层已完成 NAT |
| 外层包 | 本地节点 EIP | 出口节点 EIP | IPIP 封装头 |

### 实现方案

#### 1. EIP 分配

**文件**：`mesh/ipip.go`

EIP 是子网的第 5 个 IP（subnet + 4），例如 100.1.0.0/16 → 100.1.0.4。

**IP 保留方案**（前 10 个 IP）：
- .0 = 网络地址
- .1 = VIP（mesh 节点标识）
- .2 = hostIP（TUN 接口地址）
- .3 = GIP（DNS hijacker）
- .4 = EIP（IPIP 封装用）
- .5-.9 = 预留
- .10+ = Fake-IP 分配

**优势**：
- 避免与 Fake-IP 冲突（Fake-IP 从 .10 开始分配）
- 确定性计算，任何节点都可以从拓扑中已通告的子网计算出其他节点的 EIP
- 不需要额外的 gossip 消息
- 简单易记

```go
// CalculateEIP 计算 EIP
// EIP = subnet + 4 (例如：100.0.0.0/16 → 100.0.0.4)
func CalculateEIP(subnet *net.IPNet) net.IP {
    ip := subnet.IP.To4()
    eip := make(net.IP, 4)
    copy(eip, ip)
    eip[3] = ip[3] + 4
    return eip
}

// MeshManager.getEIPForNode 从拓扑中获取节点子网并计算 EIP
func (m *MeshManager) getEIPForNode(nodeID string) net.IP {
    subnet := m.getSubnetForNode(nodeID)
    return CalculateEIP(subnet)
}
```

#### 2. 封装逻辑（入口节点）

**文件**：`mesh/mesh.go` 或新增 `mesh/tunnel.go`

```go
// 在 mesh 发送包前检查是否需要 IPIP 封装
func (m *MeshManager) maybeEncapsulate(dstIP net.IP, packet []byte) ([]byte, error) {
    // 1. 检查规则：是否有 "via node X" 的规则
    // 2. 如果有，查找出口节点的 EIP
    // 3. 构造 IPIP 包：
    //    - 外层 IP 头：src=本地 EIP, dst=出口节点 EIP, protocol=4
    //    - 内层：原始 packet
    // 4. 返回封装后的包
}
```

#### 3. 解封装逻辑（出口节点）

**文件**：`mesh/mesh.go`

```go
// 在 mesh 收到包时检查是否是 IPIP
func (m *MeshManager) handleIPIPPacket(data []byte) error {
    // 1. 检查 IP 头的 protocol 字段是否为 4
    // 2. 剥离外层 IP 头
    // 3. 内层包注入 gVisor netstack
    //    - 调用 e.netstack.InjectInbound(innerPacket)
}
```

#### 4. 规则配置

**文件**：`config/config.go`

静态路由配置（与通告路由同层概念）：
```yaml
mesh:
  # 动态路由（通过 gossip 通告）
  advertise: ["10.0.0.0/8"]
  domain-suffixes: ["test.jf.local"]
  
  # 静态路由（本地配置，不依赖通告）
  static-routes:
    - dst: "10.0.0.0/8"
      via: "jf"  # 指定出口节点，使用 IPIP 封装
  static-domain-suffixes:
    - suffix: "internal.company.com"
      via: "jf"  # 指定出口节点，从该节点子网分配 Fake-IP
```

**设计原则**：
- **静态 IP 路由**：去程 IPIP 封装，回程正常路由（不对称，但不影响连接）
- **静态域名后缀**：DNS 解析时从目标节点子网分配 Fake-IP，正常 mesh 路由（无需 IPIP）

#### 5. 字节操作

IPIP 封装/解封装都是纯字节操作：
- **封装**：手动构造 IP 头字节（20 字节，protocol=4），拼接内层包
- **解封装**：解析外层 IP 头，剥离前 20 字节，内层包注入 netstack
- **传输**：通过现有 P2P 连接（TCP/UDP 流）发送，不需要原始 IP 能力

### 关键技术点

1. **IPIP 协议号**：IP 头中 protocol=4
2. **EIP 分配策略**：从节点子网中预留，无需通告
3. **封装位置**：mesh 层，在 P2P 发送前
4. **解封装位置**：mesh 层，在 P2P 接收后
5. **与 TUN 解耦**：TUN 层不感知 IPIP，只负责 NAT 和规则匹配

---

## 任务二：日志和连接页面拆分

### 背景

仪表盘仍然承载了太多内容：TUN 摘要、Mesh 摘要、活跃连接、日志、安全、快速操作等。需要将日志和活跃连接移到独立页面。

### 设计

#### 导航结构

```
📊 Dashboard
Runtime
  🌐 TUN
  🔷 Mesh
  📋 Logs (新)
  🔗 Connections (新)
Configuration
  📡 Subscriptions, 🔗 Proxies, 📋 Rules, 🔌 Mappings, ↗ Resolvers
Tools
  🧙 Reverse Wizard
```

#### 新增页面

1. **Logs 页面** (`/logs`)
   - 从仪表盘移入完整日志卡片
   - 独立页面，更大的显示区域
   - 支持 PiP 窗口

2. **Connections 页面** (`/connections`)
   - 从仪表盘移入活跃连接列表
   - 独立页面，支持排序/过滤
   - 支持 PiP 窗口

#### 实现

**文件变更**：
- `admin/admin.go`：新增路由和 handler
- `admin/templates/logs.html`：新建
- `admin/templates/connections.html`：新建
- `admin/templates/dashboard.html`：移除日志和连接卡片，替换为摘要
- `admin/templates/layout.html`：新增导航项
- `admin/static/app.js`：新增 PAGE_TITLES 条目
- `admin/static/i18n.js`：新增翻译键

---

## 任务三：SetReadDeadline 调研

### 背景

当前 TCP 和 UDP relay 使用 watchdog 方式实现空闲超时，代码注释说明是"avoid gVisor's SetReadDeadline lock contention that previously caused CreateEndpoint timeouts"。

需要调研：
1. gVisor 的 SetReadDeadline 是否真的有问题？
2. 是 bug（已修复？）还是使用方式不对？
3. Watchdog 是否真的必要？

### 调研计划

#### 1. 查看 gVisor 源码和 issue

- 搜索 gVisor 仓库关于 SetReadDeadline 的 issue
- 查看 gVisor netstack 的 SetReadDeadline 实现
- 确认是否有已知的锁竞争问题

#### 2. 测试 SetReadDeadline

在测试环境中：
- 使用 SetReadDeadline 替代 watchdog
- 压测观察是否有 CreateEndpoint 超时
- 监控锁竞争情况

#### 3. 决策

**如果 SetReadDeadline 可用**：
- 简化代码，移除 watchdog goroutine
- 减少 goroutine 和 channel 开销

**如果 SetReadDeadline 确实有问题**：
- 保持 watchdog 方式
- 在代码注释中详细说明原因

### 相关文件

- `tun/engine.go:1266` - `relayWithIdleTimeout`（TCP watchdog）
- `tun/engine.go:1483` - `relayUDP`（UDP watchdog）
- 注释：line 1264-1265

---

## 实施顺序

### 阶段一：调研与准备

1. **SetReadDeadline 调研**（1-2 天）
   - 查看 gVisor 源码/issue
   - 编写测试用例
   - 决策是否保留 watchdog

### 阶段二：管理面板优化

2. **日志和连接页面拆分**（2-3 天）
   - 新建 logs.html 和 connections.html
   - 从仪表盘移入相关代码
   - 更新导航和路由
   - 部署验证

### 阶段三：Mesh IPIP 隧道

3. **EIP 分配与规则配置**（2 天）
   - 设计 EIP 分配策略
   - 扩展规则配置支持 `via: node:X`

4. **IPIP 封装/解封装**（3-5 天）
   - 实现封装逻辑（入口节点）
   - 实现解封装逻辑（出口节点）
   - 集成测试

5. **端到端测试**（2 天）
   - 在 QG/VM/JF 环境测试
   - 验证策略路由
   - 性能测试

---

## 验证标准

### Mesh IPIP 隧道

1. 在入口节点配置规则：`dst:10.0.0.0/8 via node:jf`
2. 从本地访问 10.0.0.0/8 的流量
3. 验证流量通过 JF 节点出口
4. 抓包确认 IPIP 封装（外层 protocol=4）
5. 回程流量正常返回

**测试验证（已实现）**：

配置示例（VM 节点）：
```yaml
mesh:
    node-id: vm
    subnet: 100.1.0.0/16
    static-routes:
        - dst: 8.8.8.0/24
          via: jf
```

测试命令：
```bash
ping 8.8.8.8
```

预期结果：
- 回复来自 8.8.8.8（不是 100.1.0.3 gVisor）
- TTL 值合理（如 103，表示经过多跳）
- 日志显示 `[IPIP] Static route matched: dst=8.8.8.8 via=jf`

数据流：
1. VM ping 8.8.8.8 → TUN
2. HandleOutboundPacket 匹配静态路由 8.8.8.0/24 via jf
3. 计算 JF 的 EIP：100.2.255.254（从 JF 子网 100.2.0.0/16 计算）
4. IPIP 封装：外层 src=100.1.255.254 (VM EIP), dst=100.2.255.254 (JF EIP)
5. 通过 mesh 路由发送到 JF
6. JF 解封装，发送原始包到 8.8.8.8
7. 8.8.8.8 直接回复（不对称回程，无需 IPIP）

**调试日志**：
- `CheckStaticRoute` 对 8.8.8.0/24 范围的包记录 INFO 级别日志
- 便于验证静态路由匹配是否触发

### 管理面板

1. 仪表盘只显示摘要卡片
2. `/logs` 页面显示完整日志
3. `/connections` 页面显示活跃连接
4. PiP 窗口正常工作
5. SSE 实时更新正常

### SetReadDeadline

1. 明确 gVisor SetReadDeadline 的状态
2. 如果可用，简化代码
3. 如果不可用，文档化原因

---

## 风险与缓解

### Mesh IPIP 隧道

**风险**：
- IPIP 封装增加延迟和开销
- 与现有路由逻辑冲突

**缓解**：
- 性能测试
- 规则优先级设计

### 管理面板

**风险**：
- 页面拆分影响现有功能
- PiP 窗口逻辑复杂

**缓解**：
- 逐步拆分，先测试再部署
- 保留回滚能力

### SetReadDeadline

**风险**：
- 调研结论不明确
- 改动引入新 bug

**缓解**：
- 充分测试
- 灰度发布

---

## 附录

### 相关代码位置

- Mesh 路由：`mesh/topology.go`
- P2P 通信：`p2p/p2p.go`
- TUN 引擎：`tun/engine.go`
- 规则匹配：`config/config.go`
- 管理面板：`admin/admin.go`, `admin/templates/`

### 参考资料

- IPIP 协议：RFC 2003
- gVisor netstack：https://gvisor.dev/
- Phaethon mesh 设计：`docs/plans/mesh-full-topology-design.md`
