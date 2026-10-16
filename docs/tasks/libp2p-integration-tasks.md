# libp2p 集成实施任务清单

**关联设计文档**: `docs/plans/libp2p-integration-design.md`  
**创建时间**: 2026-10-14  
**总工期**: 7 周

---

## 阶段 1：基础设施搭建（1 周）

### Task 1.1: 引入 libp2p 依赖
**优先级**: P0  
**预估工时**: 2 小时  
**状态**: ⏳ 待开始

**任务描述**:
- 在 `go.mod` 中添加 libp2p 依赖
- 只引入必要的组件（core, swarm, yamux, ping）
- 验证依赖下载和编译

**验收标准**:
- [ ] `go.mod` 包含 libp2p 依赖
- [ ] `go build` 成功
- [ ] 二进制大小增长在预期范围内（< 15MB）

**实现要点**:
```go
// go.mod
require (
    github.com/libp2p/go-libp2p v0.36.0
    github.com/multiformats/go-multiaddr v0.13.0
)
```

---

### Task 1.2: 实现 TrojanTransport
**优先级**: P0  
**预估工时**: 4 小时  
**状态**: ⏳ 待开始

**任务描述**:
- 实现 `libp2p.Transport` 接口
- 封装现有的 `dialer.TrojanDialer`
- 支持通过 trojan 代理建立 libp2p 连接

**验收标准**:
- [ ] 实现 `TrojanTransport` 结构
- [ ] 实现 `Dial()` 方法
- [ ] 单元测试：通过 trojan 代理连接到 mock peer

**实现文件**: `p2p/transport_trojan.go`

**参考代码**:
```go
type TrojanTransport struct {
    proxyConfig *config.Proxy
}

func (t *TrojanTransport) Dial(ctx context.Context, addr multiaddr.Multiaddr, peerID peer.ID) (transport.CapableConn, error) {
    dialer := dialer.NewTrojanDialer(t.proxyConfig)
    conn, err := dialer.DialP2P()
    if err != nil {
        return nil, err
    }
    return t.wrapConn(conn, peerID)
}
```

---

### Task 1.3: 实现 SOCKS5Transport
**优先级**: P0  
**预估工时**: 3 小时  
**状态**: ⏳ 待开始

**任务描述**:
- 实现 `libp2p.Transport` 接口
- 封装现有的 `dialer.SOCKS5Dialer`
- 支持通过 SOCKS5 代理建立 libp2p 连接

**验收标准**:
- [ ] 实现 `SOCKS5Transport` 结构
- [ ] 实现 `Dial()` 方法
- [ ] 单元测试：通过 SOCKS5 代理连接到 mock peer

**实现文件**: `p2p/transport_socks5.go`

---

### Task 1.4: 实现 H_TunnelTransport
**优先级**: P0  
**预估工时**: 3 小时  
**状态**: ⏳ 待开始

**任务描述**:
- 实现 `libp2p.Transport` 接口
- 封装现有的 `dialer.HTunnelDialer`
- 支持通过 H_Tunnel 建立 libp2p 连接

**验收标准**:
- [ ] 实现 `H_TunnelTransport` 结构
- [ ] 实现 `Dial()` 方法
- [ ] 单元测试：通过 H_Tunnel 连接到 mock peer

**实现文件**: `p2p/transport_htunnel.go`

---

### Task 1.5: 实现 Libp2pHost 基础框架
**优先级**: P0  
**预估工时**: 6 小时  
**状态**: ⏳ 待开始

**任务描述**:
- 实现 `Libp2pHost` 结构
- 集成所有 Transport（Trojan, SOCKS5, H_Tunnel）
- 实现基本的 peer 连接管理
- 实现 stream 创建和管理

**验收标准**:
- [ ] 实现 `Libp2pHost` 结构
- [ ] 实现 `NewLibp2pHost()` 工厂方法
- [ ] 实现 `Connect()` 方法
- [ ] 实现 `NewStream()` 方法
- [ ] 实现 `Close()` 方法
- [ ] 集成测试：创建 host，连接到 mock peer，创建 stream

**实现文件**: `p2p/libp2p_host.go`

**依赖任务**: Task 1.2, 1.3, 1.4

---

## 阶段 2：P2P 层重构（2 周）

### Task 2.1: 实现 MeshStreamHandler
**优先级**: P0  
**预估工时**: 8 小时  
**状态**: ⏳ 待开始

**任务描述**:
- 实现 mesh 协议的 stream 处理
- 替代现有的 frame 处理逻辑
- 将 mesh 包注入 gVisor netstack

**验收标准**:
- [ ] 实现 `MeshStreamHandler` 结构
- [ ] 实现 `HandleStream()` 方法（处理入站 stream）
- [ ] 实现 `SendPacket()` 方法（发送 mesh 包）
- [ ] 实现消息长度前缀协议
- [ ] 单元测试：发送/接收 mesh 包

**实现文件**: `p2p/mesh_stream.go`

**参考代码**:
```go
type MeshStreamHandler struct {
    meshManager *mesh.MeshManager
    monitor     *LinkQualityMonitor
}

func (h *MeshStreamHandler) HandleStream(stream network.Stream) {
    defer stream.Close()
    peerID := stream.Conn().RemotePeer()
    
    for {
        msg, err := readMeshMessage(stream)
        if err != nil {
            h.monitor.RecordError(peerID, err)
            return
        }
        h.meshManager.HandleMeshPacket(peerID, msg)
    }
}
```

---

### Task 2.2: 移除旧的 P2P 组件
**优先级**: P0  
**预估工时**: 4 小时  
**状态**: ⏳ 待开始

**任务描述**:
- 删除 `P2PManager` 相关代码
- 删除 `DRRScheduler` 和 `dataQueues[16]`
- 删除 `writeLoop` 和 `readLoop`
- 删除手动 ACK 机制
- 删除 batch coalescing 逻辑

**验收标准**:
- [ ] 删除 `p2p/p2p.go` 中的旧代码
- [ ] 删除 `p2p/drr.go`
- [ ] 编译成功
- [ ] 确认无遗留引用

**删除文件/代码**:
- `p2p/p2p.go`: P2PManager, writeLoop, readLoop, dataQueues
- `p2p/drr.go`: DRRScheduler（整个文件）
- `p2p/ack.go`: 手动 ACK 机制（如果存在）

**依赖任务**: Task 2.1

---

### Task 2.3: 集成 Libp2pHost 到 main_engine.go
**优先级**: P0  
**预估工时**: 6 小时  
**状态**: ⏳ 待开始

**任务描述**:
- 在 `main_engine.go` 中初始化 `Libp2pHost`
- 替代原有的 `P2PManager` 初始化
- 配置 Transport 和协议处理器
- 确保 mesh 网络正常启动

**验收标准**:
- [ ] `main_engine.go` 使用 `Libp2pHost`
- [ ] 编译成功
- [ ] 单节点启动测试：程序正常启动，无 panic
- [ ] 日志显示 libp2p host 初始化成功

**实现文件**: `main_engine.go`

**依赖任务**: Task 2.1, 2.2

---

### Task 2.4: 单节点 mesh 包收发测试
**优先级**: P0  
**预估工时**: 4 小时  
**状态**: ⏳ 待开始

**任务描述**:
- 在 VM 环境部署新版本
- 测试 mesh 包的基本收发
- 验证 gVisor netstack 集成正常

**验收标准**:
- [ ] VM 环境部署成功
- [ ] mesh 包可以正常发送
- [ ] mesh 包可以正常接收
- [ ] gVisor InjectMeshPacket 正常工作
- [ ] 日志无错误

**测试方法**:
1. 部署到 VM 环境
2. 从本地 Mac 通过 QG 旁路网关访问 VM
3. 检查 VM 日志确认 mesh 包收发正常

**依赖任务**: Task 2.3

---

### Task 2.5: 多节点 mesh 路由测试
**优先级**: P0  
**预估工时**: 6 小时  
**状态**: ⏳ 待开始

**任务描述**:
- 部署到多个环境（QG, GG, JF）
- 测试多跳 mesh 路由
- 验证 RouteSelector 正常工作

**验收标准**:
- [ ] 所有节点正常连接
- [ ] mesh 路由正常工作
- [ ] 多跳路径可以正常通信
- [ ] DNS 解析正常（DNSHijacker 不受影响）

**测试场景**:
- 本地 Mac → QG → VM
- 本地 Mac → QG → JF → GG
- 本地 Mac → QG → GG → MS9

**依赖任务**: Task 2.4

---

### Task 2.6: Admin API 适配
**优先级**: P1  
**预估工时**: 8 小时  
**状态**: ⏳ 待开始

**任务描述**:
- 适配 Admin API 以支持 libp2p 数据结构
- 保持 API 接口不变（前端 UI 不需要修改）
- 适配 Peer 详情、链路质量、连接统计等

**验收标准**:
- [ ] `/api/mesh/peers` 正常工作
- [ ] `/api/mesh/topology` 正常工作
- [ ] Peer 详情显示正确
- [ ] 链路质量信息显示正确
- [ ] 前端 UI 无需修改

**需要修改的文件**:
- `server/admin.go`
- `server/admin_mesh.go`

**依赖任务**: Task 2.3

---

## 阶段 3：链路质量监控（1 周）

### Task 3.1: 实现 LinkQualityMonitor 基础结构
**优先级**: P0  
**预估工时**: 6 小时  
**状态**: ⏳ 待开始

**任务描述**:
- 实现 `LinkQualityMonitor` 结构
- 实现 `LinkMetrics` 数据结构
- 实现基本的 RTT 记录和统计

**验收标准**:
- [ ] 实现 `LinkQualityMonitor` 结构
- [ ] 实现 `LinkMetrics` 结构
- [ ] 实现 `recordRTT()` 方法
- [ ] 实现 `GetLinkQuality()` 方法
- [ ] 单元测试：RTT 统计正确

**实现文件**: `p2p/link_quality.go`

---

### Task 3.2: 实现 RTT 探测（libp2p ping）
**优先级**: P0  
**预估工时**: 4 小时  
**状态**: ⏳ 待开始

**任务描述**:
- 使用 libp2p 内置 ping 协议测量 RTT
- 实现定期探测循环
- 记录 RTT 统计信息

**验收标准**:
- [ ] 实现 `probeRTT()` 方法
- [ ] 使用 `ping.Ping()` 测量 RTT
- [ ] 定期探测（默认 5 秒间隔）
- [ ] 集成测试：RTT 测量准确

**实现文件**: `p2p/link_quality.go`

**依赖任务**: Task 3.1

---

### Task 3.3: 实现错误率统计
**优先级**: P1  
**预估工时**: 3 小时  
**状态**: ⏳ 待开始

**任务描述**:
- 统计 stream 错误（断开、超时、读写错误）
- 计算错误率
- 更新链路质量分数

**验收标准**:
- [ ] 实现 `RecordError()` 方法
- [ ] 统计总操作数和错误数
- [ ] 计算错误率
- [ ] 单元测试：错误率计算正确

**实现文件**: `p2p/link_quality.go`

**依赖任务**: Task 3.1

---

### Task 3.4: 实现链路质量广播（gossip）
**优先级**: P1  
**预估工时**: 6 小时  
**状态**: ⏳ 待开始

**任务描述**:
- 实现 `LinkQualityAdvertisement` 消息
- 定期广播自己的链路质量给邻居
- 接收并存储其他节点的链路质量通告

**验收标准**:
- [ ] 实现 `LinkQualityAdvertisement` 消息结构
- [ ] 实现 `AdvertiseLinkQuality()` 方法
- [ ] 实现 `HandleAdvertisement()` 方法
- [ ] 定期广播（默认 30 秒间隔）
- [ ] 集成测试：节点间可以共享链路质量信息

**实现文件**: `p2p/link_quality.go`

**依赖任务**: Task 3.2, 3.3

---

### Task 3.5: 实现质量分数计算
**优先级**: P1  
**预估工时**: 3 小时  
**状态**: ⏳ 待开始

**任务描述**:
- 实现综合质量分数计算算法
- 考虑 RTT、错误率、吞吐量
- 权重可调整

**验收标准**:
- [ ] 实现 `calculateQualityScore()` 方法
- [ ] 综合考虑 RTT、错误率、吞吐量
- [ ] 分数范围 0-100
- [ ] 单元测试：分数计算合理

**实现文件**: `p2p/link_quality.go`

**依赖任务**: Task 3.2, 3.3

---

## 阶段 4：路由决策集成（1 周）

### Task 4.1: RouteSelector 集成 LinkQualityMonitor
**优先级**: P0  
**预估工时**: 6 小时  
**状态**: ⏳ 待开始

**任务描述**:
- 在 `RouteSelector` 中集成 `LinkQualityMonitor`
- 查询链路质量用于路由决策
- 保留原有的路由决策逻辑

**验收标准**:
- [ ] `RouteSelector` 持有 `LinkQualityMonitor` 引用
- [ ] 路由决策时查询链路质量
- [ ] 编译成功
- [ ] 单元测试：路由决策考虑链路质量

**实现文件**: `mesh/route_selector.go`

**依赖任务**: Task 3.5

---

### Task 4.2: 实现多跳路径质量计算
**优先级**: P0  
**预估工时**: 4 小时  
**状态**: ⏳ 待开始

**任务描述**:
- 实现 `calculatePathQuality()` 方法
- 组合单跳链路质量计算多跳路径质量
- 支持不同的聚合策略（平均、最小、加权）

**验收标准**:
- [ ] 实现 `calculatePathQuality()` 方法
- [ ] 可以计算多跳路径的质量
- [ ] 单元测试：多跳路径质量计算正确

**实现文件**: `mesh/route_selector.go`

**依赖任务**: Task 4.1

---

### Task 4.3: 优化路由决策算法
**优先级**: P1  
**预估工时**: 6 小时  
**状态**: ⏳ 待开始

**任务描述**:
- 优化路由选择算法
- 综合考虑链路质量、跳数、延迟
- 避免高丢包率路径

**验收标准**:
- [ ] 路由决策考虑链路质量
- [ ] 优先选择高质量路径
- [ ] 避开高错误率路径
- [ ] 集成测试：路由决策合理

**实现文件**: `mesh/route_selector.go`

**依赖任务**: Task 4.2

---

### Task 4.4: 路由决策集成测试
**优先级**: P0  
**预估工时**: 4 小时  
**状态**: ⏳ 待开始

**任务描述**:
- 在多节点环境测试路由决策
- 验证基于链路质量的路由选择
- 对比新旧实现的性能

**验收标准**:
- [ ] 多节点环境部署成功
- [ ] 路由决策基于链路质量
- [ ] 性能对比测试完成
- [ ] 性能提升达到预期（70%+）

**测试场景**:
- 正常链路：选择最短路径
- 高丢包链路：避开高丢包路径
- 多路径：选择质量最好的路径

**依赖任务**: Task 4.3

---

## 阶段 5：测试与调优（2 周）

### Task 5.1: 性能基准测试
**优先级**: P0  
**预估工时**: 8 小时  
**状态**: ⏳ 待开始

**任务描述**:
- 设计性能测试方案
- 对比新旧实现的性能
- 对比代理模式的性能

**验收标准**:
- [ ] 单连接吞吐量测试完成
- [ ] 多连接并发测试完成
- [ ] 延迟测试完成
- [ ] 性能报告编写完成

**测试工具**:
- `iperf3`：吞吐量测试
- `ping`：延迟测试
- 自定义脚本：并发连接测试

**预期结果**:
- 多连接并发性能提升 70%+
- SSH 访问延迟降低 50%+
- 接近代理模式性能（差距 < 30%）

**依赖任务**: Task 4.4

---

### Task 5.2: 稳定性测试
**优先级**: P0  
**预估工时**: 16 小时  
**状态**: ⏳ 待开始

**任务描述**:
- 长时间运行测试（72 小时）
- 连接断开重连测试
- 节点重启测试
- 网络抖动测试

**验收标准**:
- [ ] 72 小时运行无崩溃
- [ ] 连接断开后自动重连
- [ ] 节点重启后自动恢复
- [ ] 网络抖动下保持稳定

**测试方法**:
- 持续运行 72 小时，监控内存和 CPU
- 手动断开连接，观察重连
- 重启节点，观察恢复
- 使用 `tc` 模拟网络抖动

**依赖任务**: Task 5.1

---

### Task 5.3: 全环境部署验证
**优先级**: P0  
**预估工时**: 8 小时  
**状态**: ⏳ 待开始

**任务描述**:
- 部署到所有环境（VM, QG, GG, JF, MS9, MS10）
- 验证所有功能正常
- 收集用户反馈

**验收标准**:
- [ ] 所有环境部署成功
- [ ] 所有功能正常
- [ ] 用户反馈良好
- [ ] 无严重 bug

**部署顺序**:
1. VM（开发环境）
2. QG, GG, JF（生产环境）
3. MS9, MS10（边缘环境）

**依赖任务**: Task 5.2

---

### Task 5.4: 文档更新
**优先级**: P1  
**预估工时**: 4 小时  
**状态**: ⏳ 待开始

**任务描述**:
- 更新 `agents.md` 中的架构说明
- 更新部署文档
- 编写性能测试报告
- 编写故障排查指南

**验收标准**:
- [ ] `agents.md` 更新完成
- [ ] 部署文档更新完成
- [ ] 性能测试报告编写完成
- [ ] 故障排查指南编写完成

**文档清单**:
- `agents.md`: 架构变化说明
- `docs/deployment.md`: 部署流程更新
- `docs/performance-report.md`: 性能测试结果
- `docs/troubleshooting.md`: 故障排查指南

**依赖任务**: Task 5.3

---

## 任务依赖关系

```
阶段 1: 基础设施搭建
  1.1 → 1.2, 1.3, 1.4 → 1.5

阶段 2: P2P 层重构
  1.5 → 2.1 → 2.2 → 2.3 → 2.4 → 2.5

阶段 3: 链路质量监控
  3.1 → 3.2, 3.3 → 3.4, 3.5

阶段 4: 路由决策集成
  3.5 → 4.1 → 4.2 → 4.3 → 4.4

阶段 5: 测试与调优
  4.4 → 5.1 → 5.2 → 5.3 → 5.4
```

---

## 关键里程碑

| 里程碑 | 时间 | 交付物 |
|--------|------|--------|
| M1: 基础设施完成 | 第 1 周末 | libp2p host 可以连接 peer |
| M2: P2P 层重构完成 | 第 3 周末 | mesh 包可以通过 libp2p 收发 |
| M3: 链路质量监控完成 | 第 4 周末 | 可以测量和共享链路质量 |
| M4: 路由决策集成完成 | 第 5 周末 | 路由决策基于链路质量 |
| M5: 测试完成 | 第 7 周末 | 性能提升 70%+，稳定性验证通过 |

---

## 风险缓解

### 风险 1：性能不达预期
**缓解措施**：
- 每个阶段结束后进行性能测试
- 如果阶段 2 性能不达标，先优化再进入阶段 3
- 保留回滚方案

### 风险 2：稳定性问题
**缓解措施**：
- 阶段 5 进行 72 小时稳定性测试
- 保留旧版本二进制，可以快速回滚
- 增加详细日志，便于问题定位

### 风险 3：兼容性问题
**缓解措施**：
- 强制全量升级，不做新旧版本兼容
- 升级前备份所有环境
- 制定详细的回滚计划

---

## 更新日志

| 日期 | 版本 | 更新内容 | 作者 |
|------|------|---------|------|
| 2026-10-14 | v1.0 | 初始版本 | Phaethon Team |
