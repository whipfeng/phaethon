# Mesh 直接访问控制台与状态持久化优化设计

## 元数据

- 文档类型：Plan
- 版本：v0.1.0
- 所属项目：phaethon
- 创建日期：2026-09-17

## 变更记录

| 版本 | 日期 | 变更内容 | 作者 |
|------|------|----------|------|
| v0.1.0 | 2026-09-17 | 初始版本：直接访问控制台、状态持久化优化 | Qoder |
| v0.1.1 | 2026-09-18 | 明确DNS解析保持Fake-IP方案，不改为VIP | Qoder |
| v0.1.2 | 2026-09-18 | 暂停状态持久化优化，优先确保admin handler功能稳定 | Qoder |

## 1. 背景与目标

### 1.1 当前问题

**问题一：控制台访问需要 OS 网络栈**

当前通过 mesh 网络访问节点控制台（nodeid.phn）的流程：

```
Remote app → Fake-IP → NAT → VIP → mesh → HandleMeshFrame 
→ NAT reverse → WriteMeshPacket → TUN → netstack → forwarder 
→ LookupDomain → dial 127.0.0.1:39999 → Admin Server
```

问题：
- 必须启动 TUN 和 gVisor netstack 才能访问控制台
- 包经过多次复制和转换，效率低
- 在某些场景（如无 TUN 环境）无法访问控制台

**问题二：mesh-state.json 独立于 config.yaml**

当前 mesh 状态（nodeID、subnet）持久化在独立的 `mesh-state.json` 文件中：

```json
{
  "nodeId": "123456789",
  "subnet": "100.64.1.0/24"
}
```

问题：
- 配置分散在两个文件，用户困惑
- 自动分配的值（subnet）用户看不到
- 备份/迁移需要同时处理两个文件

### 1.2 目标

1. **直接访问控制台**：mesh 层收到 nodeid.phn 的包后，直接调用 Admin Server 的 handler，不经过 OS 网络栈
2. **状态持久化统一**：将 mesh 状态（nodeID、subnet）写回 config.yaml，不再使用独立的 mesh-state.json（**暂停实施**，优先确保功能稳定）

**实施状态**：
- ✅ 直接访问控制台功能已实现
- ⏸️ 状态持久化统一暂停，保持原有 mesh-state.json 方案

## 2. 直接访问控制台设计

### 2.1 核心思路

利用 Go 的 `http.Server` 可以接受自定义 `net.Listener` 的特性：

1. AdminServer 暴露其 `http.Server` 的 handler
2. 创建一个只 yield 单个连接的 `singleConnListener`
3. 当 forwarder 检测到 `localNodeDomain=true` 时，用 gVisor 连接创建 listener，调用 `http.Server.Serve(ln)`

这样完全复用 `http.Server` 的所有逻辑（HTTP 解析、keep-alive、SSE、WebSocket 等），不需要手动解析 HTTP。

### 2.2 架构设计

```
Remote app → Fake-IP → NAT → VIP → mesh → HandleMeshFrame
→ NAT reverse → WriteMeshPacket → gVisor netstack → forwarder
→ localNodeDomain=true → AdminServer.ServeConn(gVisorConn)
→ http.Server.Serve(singleConnListener) → Admin Handler
```

关键改进：
- 不再 dial 127.0.0.1:39999（不走 OS 网络栈）
- 直接在进程内调用 Admin Server 的 handler
- 减少一次网络往返，降低延迟

**DNS 解析方案**：

nodeID.phn 域名保持使用 Fake-IP 解析，不改为 VIP。流程：
1. DNS 查询 nodeID.phn → 返回 Fake-IP（从本地或远程节点的 Fake-IP 池分配）
2. 应用连接到 Fake-IP
3. TUN engine 接收连接，通过 Fake-IP 反查原始域名
4. 检测到 localNodeDomain=true → 调用 admin handler

这样保持与现有 Fake-IP 系统的一致性，不需要特殊处理 nodeID.phn 的 DNS 解析。

### 2.3 代码设计

**AdminServer 新增方法**：

```go
// admin/admin.go

// ServeConn serves a single connection using the admin server's handler.
// This allows bypassing the OS network stack for local connections.
func (s *AdminServer) ServeConn(conn net.Conn) {
    if s.server == nil {
        conn.Close()
        return
    }
    ln := &singleConnListener{conn: conn}
    // Serve blocks until the connection is closed
    s.server.Serve(ln)
}

// singleConnListener is a net.Listener that yields a single connection.
type singleConnListener struct {
    conn net.Conn
    done atomic.Bool
}

func (l *singleConnListener) Accept() (net.Conn, error) {
    if l.done.Swap(true) {
        return nil, io.EOF
    }
    return l.conn, nil
}

func (l *singleConnListener) Close() error {
    l.done.Store(true)
    return nil
}

func (l *singleConnListener) Addr() net.Addr {
    if l.conn != nil {
        return l.conn.LocalAddr()
    }
    return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 39999}
}
```

**Forwarder 修改**：

```go
// tun/engine.go

// 在 handleTCPConnection 中，当 localNodeDomain=true 时：
if localNodeDomain {
    if e.adminHandler != nil {
        // 直接调用 Admin Server，不走 OS 网络栈
        util.LogDebug("[%s] localNodeDomain=true, serving via admin handler directly", connID)
        e.adminHandler.ServeConn(targetConn)
        return
    }
    // Fallback: dial localhost
    localIP := dialer.GetLocalIPForDial(nil)
    dialAddr := localIP.String()
    targetConn, err = dialer.DialRouteAware("tcp", net.JoinHostPort(dialAddr, strconv.Itoa(resolvedPort)))
    // ...
}
```

**TUN Engine 初始化**：

```go
// tun/engine.go

type Engine struct {
    // ... existing fields ...
    adminHandler *admin.AdminServer  // 新增
}

func NewEngine(conf *config.RuleConfiguration, opts ...Option) (*Engine, error) {
    // ... existing init ...
    
    // 设置 admin handler（由 main.go 注入）
    // e.adminHandler = adminServer
    
    return e, nil
}

// SetAdminHandler sets the admin server for direct connection handling.
func (e *Engine) SetAdminHandler(admin *admin.AdminServer) {
    e.adminHandler = admin
}
```

### 2.4 兼容性

- 如果 `adminHandler` 为 nil，fallback 到原来的 dial localhost 方式
- 不影响现有的 TUN/netstack 流程
- Admin Server 仍然可以正常监听端口，接受外部直接访问

## 3. 状态持久化优化设计

### 3.1 核心思路

将 mesh 状态（nodeID、subnet）从独立的 `mesh-state.json` 迁移到 `config.yaml`：

1. 启动时：优先从 config.yaml 读取 mesh 状态，如果没有则从 mesh-state.json 迁移
2. 自动分配后：将分配结果写回 config.yaml
3. 运行时：使用 config.yaml 中的值，不再使用 mesh-state.json

### 3.2 配置格式

```yaml
mesh:
  enabled: true
  node-id: "123456789"      # 持久化的 nodeID
  subnet: "100.64.1.0/24"   # 持久化的 subnet（自动分配或手动配置）
  # ... 其他 mesh 配置 ...
```

### 3.3 迁移逻辑

```go
// main.go

// 1. 加载 config.yaml
ruleConf, err := config.LoadRuleConfiguration(configPath)

// 2. 尝试从 mesh-state.json 迁移（一次性）
if ruleConf.Mesh.NodeID == "" {
    state, _ := mesh.LoadState(dataDir)
    if state != nil {
        if state.NodeID != "" {
            ruleConf.Mesh.NodeID = state.NodeID
            util.Logger.Printf("Mesh: migrated node-id from mesh-state.json to config.yaml")
        }
        if state.Subnet != "" {
            ruleConf.Mesh.Subnet = state.Subnet
            util.Logger.Printf("Mesh: migrated subnet from mesh-state.json to config.yaml")
        }
        // 保存更新后的 config
        config.SaveRuleConfiguration(configPath, ruleConf)
    }
}

// 3. 如果还是没有，生成新的
if ruleConf.Mesh.NodeID == "" {
    ruleConf.Mesh.NodeID, _ = mesh.GenerateNodeID()
}
if ruleConf.Mesh.Subnet == "" {
    ruleConf.Mesh.Subnet, _ = mesh.AllocateSubnet(...)
}

// 4. 保存最终配置
config.SaveRuleConfiguration(configPath, ruleConf)

// 5. 删除旧的 mesh-state.json（可选）
os.Remove(filepath.Join(dataDir, "mesh-state.json"))
```

### 3.4 配置保存

需要实现 `config.SaveRuleConfiguration` 方法：

```go
// config/config.go

// SaveRuleConfiguration saves the rule configuration to a YAML file.
func SaveRuleConfiguration(path string, conf *RuleConfiguration) error {
    data, err := yaml.Marshal(conf)
    if err != nil {
        return fmt.Errorf("marshal config fail: %w", err)
    }
    return os.WriteFile(path, data, 0644)
}
```

### 3.5 兼容性

- 首次启动时自动从 mesh-state.json 迁移
- 迁移完成后可以删除 mesh-state.json
- 如果用户手动删除 config.yaml 中的 mesh 配置，会重新生成

## 4. 实现计划

### 4.1 任务分解

1. **AdminServer.ServeConn 实现**
   - 添加 `singleConnListener` 类型
   - 添加 `ServeConn(conn net.Conn)` 方法

2. **TUN Engine 集成**
   - 添加 `adminHandler` 字段
   - 添加 `SetAdminHandler` 方法
   - 修改 `handleTCPConnection` 使用直接调用

3. **main.go 注入**
   - 创建 AdminServer 后，调用 `tunEngine.SetAdminHandler(adminServer)`

4. **配置保存实现**
   - 实现 `config.SaveRuleConfiguration`
   - 处理 YAML 序列化（保留注释、格式等）

5. **状态迁移逻辑**
   - 修改 main.go 中的 mesh 初始化逻辑
   - 从 mesh-state.json 迁移到 config.yaml
   - 删除旧的 mesh-state.json

6. **测试**
   - 单元测试：singleConnListener、ServeConn
   - 集成测试：通过 mesh 访问控制台
   - 迁移测试：从 mesh-state.json 迁移

### 4.2 部署计划

1. 本地测试通过
2. 部署到 VM 环境（Windows）测试
3. 部署到 QG 环境（Linux）验证
4. 提交代码

## 5. 风险与缓解

### 5.1 风险

1. **http.Server.Serve 阻塞**：Serve 会阻塞直到 listener 关闭
   - 缓解：singleConnListener 在连接关闭后返回 EOF

2. **YAML 序列化丢失注释**：标准 yaml.Marshal 不保留注释
   - 缓解：可以接受，或者使用更高级的 YAML 库

3. **并发访问**：多个连接同时访问控制台
   - 缓解：每个连接创建独立的 listener，http.Server 可以处理

### 5.2 回滚方案

- 如果直接调用有问题，可以禁用（adminHandler 设为 nil），fallback 到 dial localhost
- 如果迁移有问题，保留 mesh-state.json 作为备份

## 6. 参考

- [Go http.Server.Serve 文档](https://pkg.go.dev/net/http#Server.Serve)
- [Go net.Listener 接口](https://pkg.go.dev/net#Listener)
- 项目文档：`docs/plans/mesh_multi_vip_design.md`
