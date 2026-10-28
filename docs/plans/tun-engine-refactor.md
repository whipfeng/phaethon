# TUN 引擎架构重构方案

**创建时间**: 2026-10-28  
**状态**: ✅ 已完成  
**优先级**: 高

## 背景

当前 `tun.Engine` 存在架构问题：
1. **职责不清**：持有大量非 TUN 相关资源（netstack、fakeIP、dnsHijack、adminHandler、mesh 字段）
2. **耦合严重**：tun 包依赖 mesh 包和 handler 包，违反单一职责原则
3. **迁移不彻底**：连接处理逻辑已迁移到 handler 包，但 adminHandler 注入路径仍通过 tun.Engine

## 目标

1. **职责分离**：各组件只负责自己的领域
2. **依赖清晰**：减少包间耦合，通过回调/接口解耦
3. **架构合理**：符合迁移初衷，便于维护扩展

## 架构设计

### 组件职责

#### 1. tun.Manager（原 tun.Engine）
**职责**：只负责 TUN 设备 I/O 和生命周期管理

**持有的字段**：
- `device Device` — TUN 设备
- `routeMgr *RouteManager` — 路由管理器
- `tunConfig *config.TUNConfig` — TUN 配置
- `tunRunning bool` — TUN 运行状态
- `tunCloseCh chan struct{}` — 退出信号
- `tunWG sync.WaitGroup` — goroutine 等待组
- `readPackets, writePackets atomic.Uint64` — 统计

**回调函数**（解耦依赖）：
- `writeToNetstack func(data []byte) error` — 写入 netstack 的回调
- `onTUNEvent func(event string)` — TUN 事件回调（可选）

**不持有的字段**（移到 activeResources）：
- ❌ netstack、fakeIP、dnsHijack（mesh 包的）
- ❌ adminHandler（handler 包的）
- ❌ localMeshVIPs、meshSubnet、meshNetwork、modeBTable、localMeshNodeID（mesh 相关的）

#### 2. mesh.Netstack
**职责**：网络栈管理（已独立）

**当前状态**：已经是独立结构，不需要修改

#### 3. handler.ConnectionHandler
**职责**：TCP/UDP 连接处理（已独立）

**当前状态**：已经是独立结构，不需要修改

#### 4. activeResources（main.go）
**职责**：顶层资源容器，统一管理所有运行时资源

**持有的字段**：
- `tunMgr *tun.Manager` — TUN 管理器
- `netstack *mesh.Netstack` — 网络栈
- `connHandler *handler.ConnectionHandler` — 连接处理器
- `meshMgr *mesh.MeshManager` — mesh 管理器
- `adminServer *admin.AdminServer` — 管理服务器
- 其他现有字段（listeners、reverseServers 等）

**注意**：TUNResource 包装层已移除，activeResources 直接持有各组件

### 依赖关系

```
main.go (activeResources)
    ├── 创建 tun.TUNManager
    ├── 创建 mesh.Netstack
    ├── 创建 handler.ConnectionHandler
    └── 组装依赖关系：
        ├── TUNManager.writeToNetstack = netstack.InjectInbound
        ├── ConnectionHandler.adminHandler = adminServer
        └── ConnectionHandler.fakeIP = netstack.fakeIP
```

### 初始化流程

```go
func run(prev *activeResources) (*activeResources, error) {
    // 1. 创建 Manager
    mgr := tun.NewManager(tunConfig)
    
    // 2. 创建 Netstack
    netstack := mesh.NewNetstack()
    
    // 3. 创建 ConnectionHandler
    connHandler := handler.NewConnectionHandler(handler.Config{
        FakeIP: netstack.GetFakeIPPool(),
        // ... 其他配置
    })
    
    // 4. 组装依赖
    mgr.SetWriteCallback(netstack.InjectInbound)
    connHandler.SetAdminHandler(adminServer)
    
    // 5. 存储到 activeResources
    res := &activeResources{
        tunMgr: mgr,
        netstack: netstack,
        connHandler: connHandler,
        // ...
    }
    
    return res, nil
}
```

## 实施步骤

### 阶段 1：重命名和清理（低风险）✅ 已完成
1. 重命名 `tun.Engine` → `tun.Manager`
2. 重命名文件 `engine.go` → `manager.go`
3. 更新所有引用（main.go、main_engine.go、其他包）
4. 编译验证

### 阶段 2：调整 startEngine 返回值 + 移除 TUNResource（中风险）✅ 已完成
1. 修改 `startEngine` 返回 `(*tun.Manager, *mesh.Netstack, *handler.ConnectionHandler)`
2. 在 main.go 中接收多个返回值
3. 更新 activeResources 结构，添加 netstack 和 connHandler 字段
4. 调用 `connHandler.SetAdminHandler(adminServer)` 修复 bug
5. 移除 TUNResource 包装层，activeResources 直接持有 `*tun.Manager`
6. 编译验证

### 阶段 3：添加回调函数（中风险）✅ 已跳过
**说明**：原计划添加 `writeToNetstack` 回调函数解耦 netstack 依赖。但经过审视，TUN 设备与 netstack 的交互是核心功能（注入/提取数据包），保留直接引用更合理，不需要回调解耦。

### 阶段 4：清理非 TUN 字段（高风险）✅ 已完成
1. 从 Manager 中删除：
   - ❌ fakeIP、dnsHijack（通过 netstack 访问）
   - ❌ adminHandler（已在 ConnectionHandler 中）
   - ❌ localMeshVIPs、meshSubnet、meshNetwork（mesh 配置由 meshMgr 管理）
   - ❌ modeBTable、localMeshNodeID（mesh 相关）
2. 删除相关方法（15 个）：
   - SetMeshConfig、SetModeBTable、GetModeBTable
   - SetLocalMeshNodeID、SetAdminHandler
   - SetDNSHijacker、SetDNSDomainResolver、GetFakeIPPool、GetDNSHijacker
   - SetMeshNetwork、AddMeshVIP、isLocalMeshVIP、IsMeshIP
   - NetDialWithModeB、AdminHandler interface
3. 保留合理的方法：
   - ✅ SetNetstack、SetDataDir、SetPreConnectCallback
   - ✅ 所有 TUN 设备 I/O 相关方法
4. 编译验证 ✅

### 阶段 5：集成测试（高风险）✅ 已完成
1. ✅ 部署到 QG 环境
2. ✅ 验证二进制更新（hash: 7181265ee484f4d432cfc77c987d276a）
3. ✅ 验证 adminHandler 注入日志（ConnectionHandler）
4. ✅ 验证 mesh 域名访问（qg.phn → 100.0.0.12:443）
5. ✅ 验证连接处理（返回 unauthorized，证明 admin handler 正确处理）

## 风险评估

### 高风险点
1. **TUNManager 与 netstack 交互**：回调函数可能影响性能
2. **大规模字段迁移**：可能遗漏某些依赖
3. **初始化顺序**：组件间依赖可能导致循环初始化

### 缓解措施
1. **分阶段实施**：每阶段编译验证，及时发现问题
2. **保留回退路径**：git 提交每个阶段，可随时回退
3. **充分测试**：VM 环境验证后再部署 QG

### 预期工作量
- 阶段 1：0.5 小时 ✅ 已完成
- 阶段 2：1 小时 ✅ 已完成
- 阶段 3：跳过（重新评估后认为不需要）
- 阶段 4：1.5 小时 ✅ 已完成
- 阶段 5：1 小时 ✅ 已完成
- **总计**：4 小时

## 收益

1. **架构清晰**：各组件职责明确，易于理解
2. **便于维护**：修改某个组件不影响其他组件
3. **符合迁移初衷**：彻底完成连接处理逻辑的迁移
4. **扩展性强**：新增功能时，知道应该放在哪个组件

## 替代方案

### 方案 B：保持现状，只修复 adminHandler 注入
- 优点：改动小，风险低
- 缺点：架构不合理，违反迁移初衷
- **不推荐**

### 方案 C：tun.Engine 持有所有资源（大杂烩）
- 优点：不需要修改架构
- 缺点：职责不清，耦合严重
- **不推荐**

## 决策

**选择方案 A**：彻底重构，虽然工作量大，但架构合理，符合长期利益。

## 更新日志

- 2026-10-28：创建文档，完成架构设计
- 2026-10-28：完成阶段 1（重命名 Engine → Manager，engine.go → manager.go）
- 2026-10-28：完成阶段 2（调整 startEngine 返回值，修复 adminHandler 注入 bug，移除 TUNResource 包装层）
- 2026-10-28：完成阶段 4（清理 Manager 非 TUN 字段，删除 15 个 obsolete 方法）
- 2026-10-28：完成阶段 5（QG 环境部署验证，mesh 域名访问测试通过）
- 2026-10-28：重构完成，所有目标达成

## 结论

✅ **重构成功**

**核心修复**：
- adminHandler 现在正确注入到 ConnectionHandler（而非只注入到 tun.Manager）
- mesh 域名访问（如 qg.phn）不再被代理，而是由本地 admin handler 直接处理
- 架构职责清晰：tun.Manager 只负责 TUN 设备 I/O，ConnectionHandler 负责连接处理

**验证结果**：
- QG 环境部署成功（binary hash: 7181265e...）
- 日志显示 "Admin handler injected into ConnectionHandler" ✅
- curl https://qg.phn/api/status 返回 {"error":"unauthorized"} ✅（证明连接到达 admin handler）
- 无 ERROR/WARN 日志 ✅
