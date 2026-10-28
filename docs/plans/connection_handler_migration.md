# 连接处理逻辑迁移方案：从 tun/ 到 mesh/

## 背景

Commit `8a8b4f7` (2026-09-26) 尝试将 netstack 管理从 `tun/` 迁移到 `mesh/`，但 34 分钟后被回滚 (`b838597`)。随后部分重新应用 (`2e59298`)，但连接处理逻辑（`handleConn`、`handleUDP`）仍留在 `tun/engine.go`。

当前架构违反了迁移初衷：
- ❌ `tun/engine.go` 包含 `handleConn`（连接处理逻辑，与 TUN I/O 无关）
- ✅ `mesh/netstack.go` 管理 gVisor netstack
- ❌ 回调在 `tun/` 中注册，调用 `tun/` 中的 `handleConn`

正确的架构应该是：
- `tun/` 只管理 TUN 设备 I/O（readLoop, writeLoop）
- `mesh/` 管理连接处理逻辑（handleConn、handleUDP）

## 目标

将连接处理逻辑从 `tun/engine.go` 迁移到 `mesh/` 包，使 `tun/` 专注于 TUN 设备 I/O。

## 当前依赖分析

### `handleConn` 的依赖项

| 依赖 | 当前位置 | 迁移策略 |
|------|---------|---------|
| `e.fakeIP` | `tun.Engine` | 通过 `ForwarderCallbacks` 注入或移到 `mesh.Netstack` |
| `e.localMeshNodeID` | `tun.Engine` | 移到 `mesh.Netstack` 或通过回调注入 |
| `e.localMeshVIPs` | `tun.Engine` | 移到 `mesh.Netstack` 或通过回调注入 |
| `e.adminHandler` | `tun.Engine` | 通过回调注入（mesh 不应依赖 admin） |
| `e.isMeshIP()` | `tun.Engine` | 已在 `ForwarderCallbacks.IsMeshIP` 中 |
| `e.resolveForDirect()` | `tun.Engine` | 移到 `mesh/` 或通过回调注入 |
| `db.ResolveMatch()` | `db` 包 | 无需迁移，直接调用 |
| `dialer.*` | `dialer` 包 | 无需迁移，直接调用 |
| `connlog.Start()` | `connlog` 包 | 无需迁移，直接调用 |
| `relay()` | `tun.Engine` | 移到 `mesh/` 或提取为独立函数 |

### `handleUDP` 的依赖项

需要类似分析，但逻辑更简单（UDP 转发，带空闲超时）。

## 迁移方案

### 方案 A：完全迁移到 mesh/（推荐）

**核心思想**：
1. 在 `mesh/` 包创建 `ConnectionHandler` 结构体，持有连接处理所需的所有依赖
2. 将 `handleConn` 和 `handleUDP` 移到 `mesh/` 包
3. `tun.Engine` 通过 `SetCallbacks` 将连接处理委托给 `mesh.ConnectionHandler`
4. `mesh.Netstack` 持有 `ConnectionHandler` 的引用

**优点**：
- 职责清晰：`tun/` 只管理 TUN I/O，`mesh/` 管理连接处理
- 符合原迁移目标
- 便于测试和维护

**缺点**：
- 需要较大的重构
- 需要仔细处理依赖注入

**实现步骤**：

#### 阶段 1：创建 mesh.ConnectionHandler

```go
// mesh/connection_handler.go
type ConnectionHandler struct {
    fakeIP          *fakeip.Pool
    localMeshNodeID string
    localMeshVIPs   map[string]bool
    adminHandler    *admin.Handler
    modeBTable      *modeb.Table
    natTable        *nat.Table
    
    // 回调函数
    resolveForDirect func(domain string) ([]net.IP, error)
    isMeshIP         func(ip net.IP) bool
}

func (h *ConnectionHandler) HandleTCPConn(conn net.Conn, srcAddr string, srcPort int, dstAddr string, dstPort int, inbound string, modeBMapping *config.Mapping) {
    // 原 tun.Engine.handleConn 的逻辑
}

func (h *ConnectionHandler) HandleUDPConn(conn net.Conn, srcAddr, dstAddr string, dstPort int, inbound string, modeBMapping *config.Mapping) {
    // 原 tun.Engine.handleUDP 的逻辑
}
```

#### 阶段 2：修改 ForwarderCallbacks

```go
// mesh/netstack.go
type ForwarderCallbacks struct {
    HandleTCPConn func(conn net.Conn, srcAddr string, srcPort int, dstAddr string, dstPort int, inbound string, modeBMapping *config.Mapping)
    HandleUDPConn func(conn net.Conn, srcAddr, dstAddr string, dstPort int, inbound string, modeBMapping *config.Mapping)
    // ... 其他回调
}
```

保持不变，但实现从 `tun.Engine` 移到 `mesh.ConnectionHandler`。

#### 阶段 3：修改 tun.Engine

```go
// tun/engine.go
func (e *Engine) Start() error {
    // ...
    
    // 不再在这里注册回调，而是委托给 mesh.ConnectionHandler
    // mesh.ConnectionHandler 在 mesh.Netstack 初始化时设置
    
    // ...
}

// 删除 handleConn 和 handleUDP 方法
```

#### 阶段 4：修改 main_engine.go

```go
// main_engine.go
func startEngine(...) {
    // 创建 mesh.ConnectionHandler
    connHandler := &mesh.ConnectionHandler{
        fakeIP:          fakeIPPool,
        localMeshNodeID: nodeID,
        localMeshVIPs:   localVIPs,
        adminHandler:    adminHandler,
        // ...
    }
    
    // 创建 mesh.Netstack 时传入 connHandler
    netstack := mesh.NewNetstack(connHandler, ...)
    
    // 创建 tun.Engine 时传入 netstack
    engine := tun.NewEngine(netstack, ...)
    
    // 启动
    engine.Start()
}
```

### 方案 B：保留回调模式，但移动实现

**核心思想**：
1. 保持 `ForwarderCallbacks` 模式不变
2. 将 `handleConn` 和 `handleUDP` 的实现移到 `mesh/` 包
3. `tun.Engine` 在注册回调时，调用 `mesh/` 包中的函数

**优点**：
- 改动较小
- 保持现有架构

**缺点**：
- 不够彻底，`tun.Engine` 仍然知道连接处理逻辑
- 依赖注入不够清晰

**不推荐**，因为不够彻底。

### 方案 C：提取到独立包 server/

**核心思想**：
1. 创建 `server/` 包（或复用现有的）
2. 将连接处理逻辑移到 `server/`
3. `tun/` 和 `mesh/` 都委托给 `server/`

**优点**：
- 职责更清晰
- 便于复用

**缺点**：
- 需要创建新包
- 可能过度设计

**不推荐**，因为 `mesh/` 已经足够。

## 推荐方案

**方案 A：完全迁移到 mesh/**

理由：
1. 符合原迁移目标
2. 职责清晰
3. `mesh/` 已经有 `netstack.go`，添加 `connection_handler.go` 自然
4. 便于后续维护和测试

## 迁移任务清单

### 阶段 1：准备工作
- [ ] 创建 `mesh/connection_handler.go`
- [ ] 定义 `ConnectionHandler` 结构体和依赖
- [ ] 移动 `relay()` 函数到 `mesh/` 或提取为工具函数

### 阶段 2：迁移 handleConn
- [ ] 将 `handleConn` 逻辑移到 `ConnectionHandler.HandleTCPConn`
- [ ] 处理依赖注入（fakeIP、localMeshNodeID 等）
- [ ] 确保日志模块从 `tun` 改为 `mesh`

### 阶段 3：迁移 handleUDP
- [ ] 将 `handleUDP` 逻辑移到 `ConnectionHandler.HandleUDPConn`
- [ ] 处理依赖注入

### 阶段 4：修改 tun.Engine
- [ ] 删除 `tun.Engine.handleConn` 和 `handleUDP`
- [ ] 删除 `tun.Engine` 中的回调注册逻辑
- [ ] 确保 `tun.Engine` 只管理 TUN I/O

### 阶段 5：修改 main_engine.go
- [ ] 创建 `mesh.ConnectionHandler` 实例
- [ ] 传入 `mesh.Netstack` 初始化
- [ ] 确保依赖正确注入

### 阶段 6：测试验证
- [ ] 本地编译验证
- [ ] VM 环境部署验证
- [ ] QG 环境部署验证
- [ ] 测试 TUN 模式、mesh 模式、旁路网关模式
- [ ] 测试 DNS 解析、Fake-IP、规则匹配
- [ ] 测试 admin handler 本地访问

### 阶段 7：清理
- [ ] 删除临时诊断日志
- [ ] 更新设计文档
- [ ] 更新 agents.md 中的架构说明

## 风险和注意事项

1. **依赖注入复杂性**：`handleConn` 依赖较多，需要仔细设计注入方式
2. **日志模块变化**：从 `tun` 改为 `mesh`，可能影响日志过滤
3. **测试覆盖**：需要确保所有场景都测试到
4. **回滚方案**：如果迁移失败，需要能够快速回滚

## 时间估算

- 阶段 1-3：2-3 小时
- 阶段 4-5：1-2 小时
- 阶段 6：2-3 小时（包括部署和验证）
- 阶段 7：30 分钟

**总计**：6-8 小时

## 结论

迁移连接处理逻辑到 `mesh/` 是正确的架构决策，符合原迁移目标，使职责更清晰。推荐采用方案 A（完全迁移到 mesh/），虽然改动较大，但架构更合理。

迁移完成后，再继续调试原始的 jf.phn 访问问题。
