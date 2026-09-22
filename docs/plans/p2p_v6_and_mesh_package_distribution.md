# P2P 协议 v6 + Mesh 包分发

> **文档类型**：Plan  
> **版本**：2.0.0  
> **创建日期**：2026-09-21  
> **最后更新**：2026-09-22

## 变更记录

| 版本 | 日期 | 变更内容 | 作者 |
|------|------|----------|------|
| 1.0.0 | 2026-09-21 | 初始版本 | Qoder |
| 2.0.0 | 2026-09-22 | 加入版本单调递增约束、retention 策略、同步优化、域名寻址 | Qoder |

---

## 一、背景与目标

### 1.1 当前问题

1. hello 和 gossip 使用不同的数据结构，hello 携带大量无用字段（Version/Platform/Arch 等）
2. 路由建立需要等待 15s gossip 周期
3. 包分发需要手动 scp 上传到每个节点

### 1.2 目标

1. 统一 hello/gossip 数据结构，连接建立时立刻交换拓扑
2. 实现 mesh 包分发，节点间自动同步包
3. 版本单调递增，符合主流软件更新模式
4. 自动清理旧版本，节省磁盘空间

---

## 二、核心约束

### 2.1 版本单调递增

**规则**：每个 platform/arch 组合，发布版本必须高于当前最新版本。

**理由**：
- 符合主流软件更新模式（操作系统、App、固件都是版本递增）
- 无需 unpublish 操作（新版本自动替代旧版本）
- 分布式一致性自然达成（版本号是确定性的）

**实现**：
```go
// 发布时检查
currentLatest := getLatestVersion(pkg.Platform, pkg.Arch)
if p2p.CompareVersions(pkg.Version, currentLatest) <= 0 {
    return "version must be higher than current latest"
}
```

### 2.2 Retention 策略

**规则**：每个 platform/arch 保留最新的 N 个版本（N 可配置，默认 1）。

**触发时机**：
- 新版本发布成功后
- 从 peer 同步完成后

**实现**：
```go
// 按 platform/arch 分组，每组保留最新 N 个
func applyRetention(packages []Package, retention int) []Package {
    groups := groupByPlatformArch(packages)
    var toDelete []Package
    for key, pkgs := range groups {
        sort.Slice(pkgs, func(i, j int) bool {
            return CompareVersions(pkgs[i].Version, pkgs[j].Version) > 0
        })
        if len(pkgs) > retention {
            toDelete = append(toDelete, pkgs[retention:]...)
        }
    }
    return toDelete
}
```

### 2.3 NodeID 唯一性

**前提**：每个节点的 nodeID 必须全局唯一。

**理由**：
- nodeID 用于 mesh 路由、域名解析（nodeID.phn）
- 冲突无法通过协议检测/协商（需要物理链路信息，本质是唯一标识）
- 这是部署时的配置约束，不是运行时可解决的问题

---

## 三、协议变更 (v5 → v6)

### 3.1 统一数据结构

hello 和 gossip 使用同一个结构，hello 是 gossip 的超集：

```go
// mesh/topology.go
type GossipInfo struct {
    // 协议字段
    Cmd             string `json:"cmd,omitempty"`             // "hello" 或 "gossip"
    ProtocolVersion int    `json:"protocolVersion,omitempty"` // 仅 hello
    
    // 拓扑信息
    DomainSuffixes []GossipDomainSuffix  `json:"domainSuffixes,omitempty"`
    Routes         []GossipRoute         `json:"routes,omitempty"`
    ClaimedSubnets []GossipClaimedSubnet `json:"claimedSubnets,omitempty"`
}
```

### 3.2 cmd 改名

- `"mesh_gossip"` → `"gossip"`

### 3.3 hello 简化

去掉无用字段：NodeID、Version、Platform、Arch、BuildTag、Checksum、MeshNodeID、MeshVIP

sender 身份从 `claimedSubnets[hop=0].nodeId` 提取。

### 3.4 ProtocolVersion

```go
const P2PProtocolVersion = 6  // 从 5 → 6
```

版本不一致直接断连（保持现有行为）。

---

## 四、处理逻辑

### 4.1 统一拓扑处理

```go
// p2p/p2p.go
func (m *P2PManager) handleCommand(peer *Peer, payload []byte) {
    var msg map[string]interface{}
    json.Unmarshal(payload, &msg)
    
    cmd, _ := msg["cmd"].(string)
    switch cmd {
    case "hello":
        m.handleHello(peer, payload)
    case "gossip":
        m.handleGossip(peer, payload)
    }
}

// handleHello 处理 hello 消息
func (m *P2PManager) handleHello(peer *Peer, payload []byte) {
    var info GossipInfo
    json.Unmarshal(payload, &info)
    
    // 1. 版本检查
    if info.ProtocolVersion != P2PProtocolVersion {
        peer.conn.Close()
        return
    }
    
    // 2. 从 claimedSubnets[hop=0] 提取 nodeID
    nodeID := extractNodeIDFromGossip(info)
    if nodeID == "" {
        peer.conn.Close()
        return
    }
    
    // 3. 清理旧状态 + 重新注册
    if m.meshHandler != nil {
        m.meshHandler.UnregisterPeerByNodeID(nodeID)
        ps := &peerSender{peer: peer, nodeID: nodeID}
        peer.meshSender = ps
        m.meshHandler.RegisterPeer(ps)
    }
    
    // 4. 处理拓扑（共用逻辑）
    m.processGossipInfo(peer, info)
    
    peer.Status = "upToDate"
}

// handleGossip 处理 gossip 消息
func (m *P2PManager) handleGossip(peer *Peer, payload []byte) {
    var info GossipInfo
    json.Unmarshal(payload, &info)
    
    // 处理拓扑（共用逻辑）
    m.processGossipInfo(peer, info)
}

// processGossipInfo 统一的拓扑处理逻辑
func (m *P2PManager) processGossipInfo(peer *Peer, info GossipInfo) {
    if m.meshHandler != nil && peer.meshSender != nil {
        data, _ := json.Marshal(info)
        m.meshHandler.HandleTopologyGossip(peer.meshSender, data)
    }
}

// extractNodeIDFromGossip 从 gossip 提取 sender nodeID
func extractNodeIDFromGossip(info GossipInfo) string {
    for _, cs := range info.ClaimedSubnets {
        if cs.Hop == 0 {
            return cs.NodeID
        }
    }
    return ""
}
```

### 4.2 sendHello

```go
func (m *P2PManager) sendHello(peer *Peer) {
    info := GossipInfo{
        Cmd:             "hello",
        ProtocolVersion: P2PProtocolVersion,
    }
    
    // 构建 gossip 内容
    if m.meshHandler != nil {
        gossipInfo := m.meshHandler.BuildGossipInfo()
        info.DomainSuffixes = gossipInfo.DomainSuffixes
        info.Routes = gossipInfo.Routes
        info.ClaimedSubnets = gossipInfo.ClaimedSubnets
    }
    
    data, _ := json.Marshal(info)
    enqueueWrite(peer, reverse.FrameData, data)
}
```

### 4.3 sendGossip

```go
func (m *P2PManager) sendGossip(peer *Peer) {
    info := GossipInfo{
        Cmd: "gossip",
    }
    
    if m.meshHandler != nil {
        gossipInfo := m.meshHandler.BuildGossipInfo()
        info.DomainSuffixes = gossipInfo.DomainSuffixes
        info.Routes = gossipInfo.Routes
        info.ClaimedSubnets = gossipInfo.ClaimedSubnets
    }
    
    data, _ := json.Marshal(info)
    enqueueWrite(peer, reverse.FrameData, data)
}
```

### 4.4 MeshManager 新增方法

```go
// mesh/mesh.go

// BuildGossipInfo 构建当前 gossip 内容
func (m *MeshManager) BuildGossipInfo() *GossipInfo {
    // 复用 broadcastGossip 的构建逻辑
}

// UnregisterPeerByNodeID 按 nodeID 清理旧 peer
func (m *MeshManager) UnregisterPeerByNodeID(nodeID string) {
    // 查找已注册的 peer，如果 nodeID 匹配则 unregister
}
```

---

## 五、冲突处理

冲突重选 subnet 后，重发 hello 给所有 peer（清理旧状态 + 重新通告）：

```go
// mesh/mesh.go - gossipLoop
case meshEventGossip:
    // ... 处理 gossip ...
    if m.checkSubnetConflict() {
        m.recomputeRoutes()
        // 重发 hello（带新 subnet/VIP + gossip）给所有 peer
        m.p2p.ResendHelloToAll()
        m.broadcastGossip()
    }

// p2p/p2p.go
func (m *P2PManager) ResendHelloToAll() {
    m.mu.RLock()
    defer m.mu.RUnlock()
    for _, peer := range m.peers {
        m.sendHello(peer)
    }
}
```

---

## 六、Mesh 包分发

### 6.1 前提

hello/gossip 统一后，peer 注册完成时路由已建立，可以立刻通过 mesh 互访。

### 6.2 Mesh HTTP Client

通过 `engine.NetDial` 创建 mesh HTTP client，使用 mesh 域名寻址：

```go
// admin/admin.go
func (s *AdminServer) newMeshHTTPClient() *http.Client {
    return &http.Client{
        Transport: &http.Transport{
            DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
                return s.meshDialFn(network, addr)  // engine.NetDial
            },
            TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
        },
        Timeout: 30 * time.Second,
    }
}
```

### 6.3 发布推送

```go
// admin/package.go - apiPackagePublish 成功后
go s.DistributePackage(pkgFileData)

// admin/admin.go
func (s *AdminServer) DistributePackage(pkgData []byte) {
    for _, peer := range s.peerLister() {
        go func(nodeID string) {
            domain := mesh.NodeDomain(nodeID)  // nodeID.phn
            url := fmt.Sprintf("https://%s:%d/api/packages/receive", domain, s.adminPort)
            s.meshHTTPClient.Post(url, "application/octet-stream", bytes.NewReader(pkgData))
        }(peer.NodeID)
    }
}
```

### 6.4 链路建立触发同步

peer 注册完成时触发同步（不是启动时）：

```go
// main.go
resources.meshMgr.OnPeerRegistered = func(nodeID string) {
    adminSrv.SyncFromPeer(nodeID)
}

// admin/admin.go
func (s *AdminServer) SyncFromPeer(nodeID string) {
    // 延迟 2s 等待路由稳定
    time.Sleep(2 * time.Second)
    go s.syncFromPeer(nodeID)
}

func (s *AdminServer) syncFromPeer(nodeID string) {
    domain := mesh.NodeDomain(nodeID)
    url := fmt.Sprintf("https://%s:%d/api/packages", domain, s.adminPort)
    
    // 1. GET /api/packages 获取 peer 的包列表
    // 2. 按 platform/arch 分组，每组取最新 N 个 → 目标集合
    // 3. 对比本地：目标集合中有、本地没有的 → 下载
    // 4. 如果下载全部成功 → 删除本地有但不在目标集合中的
    // 5. 如果下载失败 → 保留旧版本，下次再试
}
```

### 6.5 定时兜底同步

5 分钟定时任务，处理同步失败的情况：

```go
// main.go
go func() {
    ticker := time.NewTicker(5 * time.Minute)
    defer ticker.Stop()
    for {
        select {
        case <-ticker.C:
            for _, peer := range resources.meshMgr.GetPeers() {
                adminSrv.SyncFromPeer(peer.NodeID)
            }
        case <-resources.meshMgr.CloseCh():
            return
        }
    }
}()
```

### 6.6 接收端点

```go
// admin/package.go
func (s *AdminServer) apiPackageReceive(w http.ResponseWriter, r *http.Request) {
    // 1. 读取 body（.pkg 文件）
    // 2. signing.VerifyPkgBytes → 验证签名
    // 3. 检查版本 > 当前最新版本（版本单调递增）
    // 4. 保存 .pkg + meta
    // 5. 标记 published
    // 6. BumpVersion("packages")
    // 7. 应用 retention 策略，删除旧版本
    // 8. 继续推送给其他邻居（flood fill）
    go s.DistributePackage(pkgData)
}
```

### 6.7 同步逻辑详解

```
同步流程（从 peer 同步包）：

1. GET /api/packages 获取 peer 的包列表（metadata）
   返回：[{id, platform, arch, version, published}, ...]

2. 按 platform/arch 分组，每组取最新 N 个（retention 配置）
   示例（retention=2）：
   Peer 有: linux/amd64 v1.2.1, v1.2.2, v1.2.3, v1.2.4
   目标集合: v1.2.3, v1.2.4（最新 2 个）

3. 对比本地：
   本地有: v1.2.1
   目标集合中有、本地没有: v1.2.3, v1.2.4 → 下载

4. 下载完成后（全部成功）：
   本地有但不在目标集合中: v1.2.1 → 删除

5. 如果下载失败：
   保留旧版本，下次再试
```

### 6.8 路由注册

```go
// admin/admin.go - registerRoutes
mux.HandleFunc("/api/packages/receive", s.apiPackageReceive)

// isPublicPath 添加（包端点不需要认证，签名即信任）
func isPublicPath(path string, method string) bool {
    if method == http.MethodGet {
        if path == "/api/packages" { return true }
        if strings.HasPrefix(path, "/api/packages/") && strings.HasSuffix(path, "/download") { return true }
    }
    if method == http.MethodPost && path == "/api/packages/receive" { return true }
    return false
}
```

### 6.9 组装

```go
// main.go
adminServer.SetPeerLister(func() []admin.PeerBrief {
    // 返回已连接 peer 的 NodeID 列表
})
adminServer.SetMeshDialFn(func(network, addr string) (net.Conn, error) {
    return tunRes.engine.NetDial(network, addr)
})
adminServer.SetAdminPort(conf.AdminPort)
```

---

## 七、文件变更清单

| 文件 | 变更 |
|------|------|
| `mesh/topology.go` | GossipInfo 加 Cmd、ProtocolVersion 字段 |
| `p2p/p2p.go` | 删除 HelloMsg，用 GossipInfo 替代；handleHello 改为提取 nodeID + UnregisterPeer → RegisterPeer + 处理拓扑；handleGossip 处理拓扑；processGossipInfo 共用逻辑；sendHello/sendGossip 构建 GossipInfo；ProtocolVersion → 6；新增 ResendHelloToAll |
| `mesh/mesh.go` | 新增 BuildGossipInfo()、UnregisterPeerByNodeID()、OnPeerRegistered 回调、CloseCh()；冲突后调 ResendHelloToAll；gossipLoop 中 RegisterPeer 后调 broadcastGossip |
| `admin/admin.go` | 新增 PeerBrief、meshHTTPClient、peerLister、meshDialFn、adminPort；新增 SetPeerLister、SetMeshDialFn、SetAdminPort；更新 isPublicPath 支持 method 参数；包端点无需认证 |
| `admin/package.go` | 新增 apiPackageReceive；apiPackagePublish 后调 DistributePackage；SyncFromPeer 实现同步逻辑（目标集合、下载、retention）；版本单调递增检查 |
| `main.go` | 组装：SetPeerLister、SetMeshDialFn、SetAdminPort、OnPeerRegistered 回调、5 分钟定时同步 |
| `docs/plans/p2p_v6_and_mesh_package_distribution.md` | 本文档 |

---

## 八、验证与部署策略

### 8.1 分阶段部署

**第一阶段：VM + QG 环境验证**
1. 在 VM 和 QG 环境部署新版（P2P v6）
2. 验证协议升级、路由建立、冲突恢复
3. 验证包分发功能（VM 发布 → QG 接收）
4. 确认稳定运行 24h+

**第二阶段：泛洪到其他环境**
1. VM + QG 验证通过后，部署到 MS9、MS10、GG 等环境
2. 验证跨多节点的包分发（flood fill）
3. 监控 mesh 网络稳定性

### 8.2 验证方案

#### 协议升级验证
1. VM 和 QG 建立 P2P 连接 → 检查日志确认 hello 携带 gossip → 路由立刻建立（不等 15s）
2. 版本不一致时（一个 v5 一个 v6）→ 检查日志确认断连

#### 冲突恢复验证
1. 模拟 subnet 冲突（手动配置相同 subnet）→ 检查重选后 hello 重发 → 路由更新
2. 检查中间节点日志确认转发了矛盾的声明（双头场景）

#### 包分发验证
1. VM 发布包 → 检查 QG 日志确认收到 push → 包列表自动更新
2. 重启 QG → 检查 peer 注册后自动同步 VM 的包
3. 检查签名验证失败时拒绝接收
4. 检查版本单调递增：发布低版本被拒绝
5. 检查 retention：旧版本自动删除

---

## 九、设计决策总结

| 问题 | 决策 | 理由 |
|------|------|------|
| 版本管理 | **单调递增** | 符合主流软件更新模式，无需 unpublish |
| 删除操作 | **自动 retention** | 新版本到达后自动清理旧版本，节省磁盘 |
| 同步触发 | **链路建立时** | 无时间窗口漏消息，新 peer 立刻同步 |
| 兜底机制 | **5 分钟定时** | 处理同步失败的情况 |
| 寻址方式 | **mesh 域名** | nodeID.phn，不依赖 VIP |
| 认证方式 | **包端点无需认证** | 签名即信任，简化 mesh 通信 |
| 数据结构 | **统一 GossipInfo** | hello 是 gossip 超集，减少冗余 |
| 协议版本 | **v5 → v6** | 不兼容变更，版本不一致断连 |
