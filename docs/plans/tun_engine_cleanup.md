# tun.Engine 职责清理方案

**状态**：设计中  
**创建时间**：2026-10-28  
**目标**：tun.Engine 只负责 TUN 设备 I/O 和生命周期管理

## 问题背景

当前 tun.Engine 包含大量不属于 TUN 设备管理的方法，违反了单一职责原则：
- DNS 解析和劫持
- 网络拨号（NetDial）
- Mesh 配置管理
- h_tunnel 端点管理
- Netstack 生命周期

这些方法应该迁移到更合适的模块中。

## 设计原则

1. **tun.Engine 只负责 TUN 设备**：
   - TUN 设备读写（readLoop/writeLoop）
   - TUN 设备生命周期（StartTUN/StopTUN）
   - TUN 设备状态（Stats/RouteSnapshot）
   - TUN 地址配置（ConfigureMeshAddresses，可保留）

2. **mesh.Netstack 负责网络栈**：
   - DNS 服务（DNSHijacker/FakeIPPool）
   - 网络拨号（NetDial/NetDialWithModeB/NetDialV2）
   - 域名解析（ResolveDomain/ResolveDomainViaDNS）
   - Mesh 配置（VIP/NodeID/ModeBTable）
   - h_tunnel 端点（AddHTunnelEndpoint/RemoveHTunnelEndpoint）
   - Netstack 生命周期（Start/Stop）

3. **main_engine.go 负责组装**：
   - 创建和初始化各组件
   - 设置组件间的依赖关系
   - 不再通过 tun.Engine 间接访问 netstack

## 迁移清单

### 1. DNS 相关（移到 mesh.Netstack）

| 方法 | 当前位置 | 目标位置 | 说明 |
|------|---------|---------|------|
| `SetDNSHijacker()` | tun.Engine | mesh.Netstack | DNS 劫持器设置 |
| `GetDNSHijacker()` | tun.Engine | mesh.Netstack | DNS 劫持器获取 |
| `SetDNSDomainResolver()` | tun.Engine | mesh.Netstack | DNS 域名解析器设置 |
| `ResolveDomain()` | tun.Engine | mesh.Netstack | 域名解析 |
| `ResolveDomainViaDNS()` | tun.Engine | mesh.Netstack | 通过指定 DNS 服务器解析 |
| `GetFakeIPPool()` | tun.Engine | mesh.Netstack | FakeIP 池获取 |

**迁移步骤**：
1. 在 mesh.Netstack 中实现这些方法（如果还没有）
2. 修改调用方，直接调用 `netstack.XXX()` 而不是 `engine.XXX()`
3. 从 tun.Engine 中删除这些方法

### 2. 网络拨号（移到 mesh.Netstack）

| 方法 | 当前位置 | 目标位置 | 说明 |
|------|---------|---------|------|
| `NetDial()` | tun.Engine | mesh.Netstack | 基础网络拨号 |
| `NetDialWithModeB()` | tun.Engine | mesh.Netstack | Mode B 网络拨号 |
| `NetDialV2()` | tun.Engine | mesh.Netstack | V2 网络拨号 |

**迁移步骤**：
1. 在 mesh.Netstack 中实现这些方法（如果还没有）
2. 修改 dialer 包，直接调用 `netstack.XXX()` 而不是 `engine.XXX()`
3. 从 tun.Engine 中删除这些方法
4. 修改 `dialer.GlobalNetstackDialWithModeBFunc` 等全局变量，指向 netstack 方法

### 3. Mesh 配置（移到 mesh.Netstack）

| 方法 | 当前位置 | 目标位置 | 说明 |
|------|---------|---------|------|
| `SetMeshConfig()` | tun.Engine | mesh.Netstack | Mesh VIP 配置 |
| `SetModeBTable()` | tun.Engine | mesh.Netstack | Mode B 表设置 |
| `GetModeBTable()` | tun.Engine | mesh.Netstack | Mode B 表获取 |
| `SetPreConnectCallback()` | tun.Engine | mesh.Netstack | 预连接回调 |
| `SetLocalMeshNodeID()` | tun.Engine | mesh.Netstack | 本地 Mesh Node ID |
| `SetAdminHandler()` | tun.Engine | main_engine.go | Admin handler 设置 |
| `isLocalMeshVIP()` | tun.Engine | mesh.Netstack | 判断是否本地 VIP |
| `AddMeshVIP()` | tun.Engine | mesh.Netstack | 添加 Mesh VIP |
| `RemoveMeshVIP()` | tun.Engine | mesh.Netstack | 移除 Mesh VIP |
| `ResolveForDirect()` | tun.Engine | mesh.Netstack | 直连地址解析 |
| `AddMeshRoute()` | tun.Engine | mesh.Netstack | 添加 Mesh 路由 |
| `SetMeshNetwork()` | tun.Engine | mesh.Netstack | Mesh 网络设置 |
| `IsMeshIP()` | tun.Engine | mesh.Netstack | 判断是否 Mesh IP |

**迁移步骤**：
1. 在 mesh.Netstack 中实现这些方法（如果还没有）
2. 修改调用方，直接调用 `netstack.XXX()` 而不是 `engine.XXX()`
3. 从 tun.Engine 中删除这些方法
4. `SetAdminHandler()` 移到 main_engine.go，因为 admin handler 不是 netstack 的概念

### 4. Netstack 生命周期（移到 mesh.Netstack）

| 方法 | 当前位置 | 目标位置 | 说明 |
|------|---------|---------|------|
| `StartStack()` | tun.Engine | mesh.Netstack.Start() | 启动 netstack |
| `StopStack()` | tun.Engine | mesh.Netstack.Stop() | 停止 netstack |
| `SetNetstack()` | tun.Engine | 删除 | 不需要设置，main 直接持有 |
| `GetNetstack()` | tun.Engine | 删除 | 不需要获取，main 直接持有 |

**迁移步骤**：
1. 确保 mesh.Netstack 有 Start() 和 Stop() 方法
2. 修改 tun.Engine.Start() 和 Stop()，直接调用 netstack 的对应方法
3. 从 tun.Engine 中删除 StartStack() 和 StopStack()
4. 删除 SetNetstack() 和 GetNetstack()，main_engine.go 直接持有 netstack 引用

### 5. h_tunnel 管理（删除 tun.Engine 包装）

| 方法 | 当前位置 | 目标位置 | 说明 |
|------|---------|---------|------|
| `AddHTunnelEndpoint()` | tun.Engine | mesh.Netstack | 添加 h_tunnel 端点 |
| `RemoveHTunnelEndpoint()` | tun.Engine | mesh.Netstack | 移除 h_tunnel 端点 |
| `HTunnelEndpoint()` | tun.Engine | mesh.Netstack | 获取 h_tunnel 端点 |

**迁移步骤**：
1. 确保 mesh.Netstack 有这些方法（已经存在）
2. 修改调用方，直接调用 `netstack.XXX()` 而不是 `engine.XXX()`
3. 从 tun.Engine 中删除这些方法

### 6. 保留在 tun.Engine 的方法

| 方法 | 说明 |
|------|------|
| `ConfigureMeshAddresses()` | TUN 设备地址配置，可保留 |
| `Write()` | TUN 设备写入 |
| `WriteMeshPacket()` | TUN 设备写入 mesh 包 |
| `readLoop()` | TUN 设备读取循环 |
| `meshWriteLoop()` | TUN 设备 mesh 写入循环 |
| `StartTUN()` | 启动 TUN 设备 |
| `StopTUN()` | 停止 TUN 设备 |
| `Start()` | 启动 engine（调用 StartStack + StartTUN） |
| `Stop()` | 停止 engine（调用 StopTUN + StopStack） |
| `IsTUNRunning()` | TUN 设备状态 |
| `IsEnabled()` | TUN 配置状态 |
| `Stats()` | TUN 统计信息 |
| `RouteSnapshot()` | 路由快照 |
| `DHCPLeaseSnapshot()` | DHCP 租约快照 |
| `UpdateDHCPStaticBindings()` | DHCP 静态绑定 |
| `TUNInterfaceIndex()` | TUN 接口索引 |
| `TUNInterfaceIP()` | TUN 接口 IP |
| `PhysicalInterfaceIndex()` | 物理接口索引 |
| `logEvent()` | 日志记录 |
| `Logs()` | 获取日志 |
| `notifyStatsChanged()` | 统计变更通知 |

## 实施阶段

### 阶段 1：准备 mesh.Netstack
- [ ] 确保 mesh.Netstack 有所有需要的方法
- [ ] 实现缺失的方法
- [ ] 编译验证

### 阶段 2：迁移 DNS 相关
- [ ] 修改调用方
- [ ] 删除 tun.Engine 方法
- [ ] 编译验证

### 阶段 3：迁移网络拨号
- [ ] 修改 dialer 包
- [ ] 修改全局变量指向
- [ ] 删除 tun.Engine 方法
- [ ] 编译验证

### 阶段 4：迁移 Mesh 配置
- [ ] 修改调用方
- [ ] 删除 tun.Engine 方法
- [ ] 编译验证

### 阶段 5：迁移 Netstack 生命周期
- [ ] 修改 tun.Engine.Start/Stop
- [ ] 删除 StartStack/StopStack
- [ ] 删除 SetNetstack/GetNetstack
- [ ] 编译验证

### 阶段 6：删除 h_tunnel 包装
- [ ] 修改调用方
- [ ] 删除 tun.Engine 方法
- [ ] 编译验证

### 阶段 7：部署验证
- [ ] VM 环境部署
- [ ] QG 环境部署
- [ ] 功能验证

### 阶段 8：清理和文档
- [ ] 清理诊断日志
- [ ] 更新文档
- [ ] 提交代码

## 风险和注意事项

1. **保持功能不变**：迁移过程中不能改变任何功能行为
2. **逐步迁移**：每个阶段独立验证，避免大规模改动
3. **调用方修改**：需要修改所有调用方，包括 main_engine.go、dialer 包等
4. **测试覆盖**：确保有足够的测试覆盖，验证迁移后的功能
5. **回滚方案**：如果迁移失败，可以快速回滚到之前的版本

## 成功标准

- [ ] tun.Engine 只包含 TUN 设备相关的方法
- [ ] 所有 DNS/拨号/Mesh 配置方法都在 mesh.Netstack
- [ ] main_engine.go 直接持有 netstack 引用
- [ ] 编译通过，无错误
- [ ] VM 环境功能正常
- [ ] QG 环境功能正常
- [ ] 无回归问题
