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
| 2.1.0 | 2026-09-23 | mesh 数据帧传输设计（drop-tail + 大缓冲 + 分帧型写超时）；同步删除改为与本地 merge 后取 top N；分发 URL 去掉端口（mesh localNodeDomain 机制端口无关） | Qoder |
| 2.1.1 | 2026-09-23 | 明确包唯一性语义：唯一性由 (version, platform, arch) 元组决定，ID 仅作本地文件名（随机生成，跨节点不一致）；syncFromPeer 的"本地是否已有"判断与 retention 删除改用该元组对比，receive 去重按 platform+arch+version 三元组；修复随机 ID 被当作唯一键导致的同版本包重复繁殖 | Qoder |
| 2.1.2 | 2026-09-23 | 路由/域名副本选择改为按经 peer 的真实距离（claims hop）取最小，via hop 同源标注；移除 claims hop≤20 过滤（邻居互认已足够）；修复先到先得选择导致的 GG→qg→vm→GG 幽灵路由自持环与 10.161.88.0/24 黑洞 | Qoder |

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

通过 `dialer.MeshDial` 创建 mesh HTTP client，复用 Mode B 入口的连接方式：

**关键点**：
- 使用 `dialer.MeshDial` 而非 `engine.NetDial`
- `dialer.MeshDial` 内部会：
  1. 用 `engine.ResolveDomain` 解析域名得到 Fake-IP（走 netstack DNS 流程）
  2. 用 `engine.NetDialWithModeB` 拨号 Fake-IP
- 连接源地址是 GIP (.3)，回程包到 GIP → 投递给 gVisor netstack → 匹配到连接的 socket
- 这样 mesh HTTP client 的连接就和 Mode B 入口一样，走 gVisor netstack

```go
// admin/admin.go
func (s *AdminServer) newMeshHTTPClient() *http.Client {
    return &http.Client{
        Transport: &http.Transport{
            DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
                // 使用 dialer.MeshDial 而非 engine.NetDial
                // MeshDial 内部会解析域名（得到 Fake-IP）+ NetDialWithModeB（GIP 源地址）
                host, port, _ := net.SplitHostPort(addr)
                portNum, _ := strconv.Atoi(port)
                return dialer.MeshDial(host, portNum, "", "admin-mesh", nil)
            },
            TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
        },
        Timeout: 30 * time.Second,
    }
}
```

**为什么不用 engine.NetDial**：
- `engine.NetDial` 用 VIP 或 hostIP 作为源地址
- VIP (.1) 只用于 NAT 回程，没有 NAT 记录会被丢包
- hostIP (.2) 回程写到 TUN → OS，但连接在 gVisor netstack，收不到
- GIP (.3) 回程投递给 gVisor netstack，才能匹配到连接的 socket

### 6.3 发布推送

```go
// admin/package.go - apiPackagePublish 成功后
go s.DistributePackage(pkgFileData)

// admin/admin.go
func (s *AdminServer) DistributePackage(pkgData []byte) {
    for _, peer := range s.peerLister() {
        go func(nodeID string) {
            domain := mesh.NodeDomain(nodeID)  // nodeID.phn
            // 不带端口：mesh 访问走 netstack localNodeDomain 直连 admin handler，端口无关
            url := fmt.Sprintf("https://%s/api/packages/receive", domain)
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
    // 不带端口：mesh 访问走 netstack localNodeDomain 直连 admin handler，端口无关
    url := fmt.Sprintf("https://%s/api/packages", domain)
    
    // 1. GET /api/packages 获取 peer 的包列表
    // 2. 按 platform/arch 维度，peer 列表与本地列表 merge，
    //    按版本降序取前 N 个（N=packageRetention）→ 保留集合
    // 3. 保留集合中有、本地没有的 → 下载
    // 4. 下载全部成功 → 删除本地不在保留集合中的（其余的才删）
    // 5. 下载失败 → 保留旧版本，下次再试
    //
    // 注意：目标集必须与本地 merge 后再决定删除。
    // 不能只按单个 peer 的列表构建目标集——peer 包列表为空（或缺失某些
    // platform/arch 组）时会把本地其他 peer 同步过来的包全部误删。
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

**包唯一性语义（去重的基础）**：包的唯一性由 **(version, platform, arch) 元组**决定——同一平台/架构下，一个版本号唯一对应一个包。ID 只是本地文件名（随机生成，**跨节点不一致**），不参与唯一性判断。

```
pkgKey = platform + "/" + arch + "@" + version    // 例如 linux/amd64@v0.7.4
```

因此：
- `syncFromPeer` 的"本地是否已有"判断与 retention 删除都必须按 pkgKey 对比。按 ID 对比会因随机 ID 误判"本地缺失"，导致同版本包被反复下载、重复繁殖；
- retention 删除时，同 pkgKey 的多份重复文件只保留一份（清理历史遗留的重复）；
- receive 端点按 platform+arch+version 三元组去重（不能只按 version——同版本不同平台的包会被误判为已存在）。

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

### 6.8 热替换（Hot Swap）

**设计原则**：
- 看门狗是启动器（launcher），不是业务程序
- 看门狗逻辑足够简单（找 pkg → 提取 → 启动 → 监控），可以固化，不需要升级
- 类似 BIOS/bootloader：只负责加载和启动，本身不包含业务逻辑
- Worker 包含所有业务逻辑，通过热替换升级

**包分发流程**：
1. 节点 A 上传包 → 保存 pkg 到工作目录 → 通知邻居 B、C（"我有新包了"）
2. 节点 B 收到通知 → 查询 A 的包列表 → 决定要不要下载 → 调用 `syncFromPeer` 从 A 拉取
3. 节点 B 下载完成 → 验证签名 OK → 保存 pkg 到工作目录 → 通知自己的邻居 D、E

**热替换触发点**（2个）：
1. `apiPackageUpload` - 本地上传后，保存成功
2. `syncFromPeer` - 从 peer 拉取包，下载验证成功后

**注意**：`apiPackageReceive` 只接收通知（不接收包本身），不触发热替换。收到通知后调用 `syncFromPeer` 去拉取包。

**热替换逻辑**：
- Worker 检测到比自己高的版本，直接退出
- 看门狗发现 worker 退出，重新查找最新 pkg
- 看门狗提取最新 binary（如果还没提取过）
- 看门狗启动新 worker，工作目录保持不变
- 看门狗自身不升级（逻辑固化，不需要升级）

**目录结构**：
```
工作目录/（例如 /root/ 或 /home/layer4/phaethon-gg/）
├── phaethon                    # 看门狗 binary（固化，不升级）
├── config.yaml                 # 主配置文件
└── data/                       # 所有运行时数据
    ├── packages/               # pkg 文件（分发的 source of truth）
    │   ├── xxx.pkg
    │   └── xxx.json
    ├── worker/                 # 提取的 worker binary（缓存，避免重复提取）
    │   ├── phaethon-v0.3.5
    │   └── phaethon-v0.3.6
    ├── state/                  # 运行时状态
    │   ├── mesh-state.json     # mesh 网络状态
    │   ├── phaethon.pid        # PID 文件
    │   └── stopped             # 优雅停止标记
    ├── logs/                   # 日志文件
    │   ├── phaethon.log        # 主日志
    │   └── access.log          # 访问日志
    ├── cache/                  # 缓存（可安全删除）
    │   └── dns-cache.db
    └── certs/                  # 证书和密钥
        ├── admin.crt
        └── admin.key
```

**设计原则**：
- **单目录结构**：工作目录包含所有内容，便于备份、迁移、容器化
- **配置在根**：config.yaml 在工作目录根，便于查找和编辑
- **数据集中**：所有运行时数据在 data/ 下，便于清理和管理
- **跨平台**：Windows/Linux 都适用，不依赖系统特定路径

**pkg 存放位置**：
- 放在 `data/packages/` 目录（相对于看门狗的工作目录）
- pkg 文件是分发的 source of truth，包含签名、元数据、binary

**worker binary 存放位置**：
- 提取到 `data/worker/` 目录（相对于看门狗的工作目录）
- 例如：`data/worker/phaethon-v0.3.5`
- 不使用 `/tmp`，避免被系统自动清理，统一管理
- 如果 binary 已存在，直接复用，不重复提取
- 只保留最近 5 个版本，自动清理旧版本

**状态文件位置**：
- mesh-state.json → `data/state/mesh-state.json`
- PID 文件 → `data/state/phaethon.pid`
- 停止标记 → `data/state/stopped`

**日志文件位置**：
- 主日志 → `data/logs/phaethon.log`
- 支持日志轮转，保留最近 7 天或 100MB

**证书文件位置**：
- 证书和密钥 → `data/certs/`
- 便于备份和权限管理

**看门狗启动流程**：
1. 从 `./data/packages/` 查找最高版本的 pkg 文件
2. **平台/架构匹配**：只考虑平台/架构与当前编译时标识符（`main.Platform` 和 `main.Arch`）匹配的 pkg。使用编译时标识符而非运行时标识符（`runtime.GOOS`/`runtime.GOARCH`），以支持 Windows 7 兼容版本等特殊平台
3. **版本比较**：pkg 版本高于看门狗自身版本（ldflags 注入的 `main.Version`，`p2p.CompareVersions` 比较）时才使用 pkg；否则使用看门狗自身二进制。防止旧 pkg 将手动部署的新版本降级，保证「pkg 分发」和「直接部署」两条升级路径都可靠
4. 检查 `data/worker/` 是否已有对应版本的 binary
5. 如果没有，从 pkg 文件中提取 binary 到 `data/worker/` 目录（必须提取，不能直接执行 zip 中的文件）
6. 启动 binary 时，**工作目录设置为看门狗的工作目录**（即 `.` 目录，不是 `data/packages/`）
7. 配置文件（`config.yaml`）在工作目录根，确保 binary 能正确读取

**为什么看门狗不需要升级**：
- 看门狗是启动器，类似 BIOS/bootloader，逻辑简单且稳定
- 只负责：查找 pkg、版本比较、提取 binary、启动进程、监控心跳
- 不包含业务逻辑（代理、路由、mesh 等）
- 即使 pkg 格式变化，可以做向后兼容
- 服务管理器（OpenRC/systemd）是稳定基座，负责重启看门狗（如果需要）

**架构设计原则：基座固化，上层可变**：

这是经典的计算机架构分层模式，基座层简单稳定，上层复杂多变：

| 基座层（固化） | 上层（可变） | 说明 |
|--------------|------------|------|
| 硬件 | 软件 | 硬件稳定，软件可替换 |
| OS 内核 | 应用程序 | 内核稳定，应用变化 |
| Bootloader (GRUB/UEFI) | 操作系统 | 启动器固化，OS 可替换 |
| 运行时 (JVM/Node.js) | 业务代码 | 运行时稳定，用户代码变化 |
| 框架 | 业务逻辑 | 框架稳定，业务逻辑变化 |
| 数据库引擎 (MySQL/PostgreSQL) | 数据/Schema | 引擎稳定，数据结构变化 |
| **看门狗 (phaethon)** | **Worker (phaethon)** | **启动器固化，业务程序变化** |

**共同点**：
- 基座层简单、稳定、经过充分验证
- 上层复杂、多变、包含业务逻辑
- 基座提供能力，上层使用能力

我们的设计遵循这个原则：看门狗作为基座（启动器）固化，Worker 作为上层（业务程序）通过热替换升级。

**实现**：
```go
// admin/package.go
func (s *AdminServer) checkForNewerVersion(contents *signing.PkgContents) {
    // 1. 检查平台/架构是否匹配（使用编译时标识符，不是运行时）
    if s.GetPlatform == nil || s.GetArch == nil {
        return
    }
    currentPlatform := s.GetPlatform()
    currentArch := s.GetArch()
    if contents.Meta.Platform != currentPlatform || contents.Meta.Arch != currentArch {
        return
    }
    
    // 2. 检查版本是否更高
    if s.GetCurrentVersion == nil {
        return
    }
    currentVersion := s.GetCurrentVersion()
    if p2p.CompareVersions(contents.Meta.Version, currentVersion) <= 0 {
        return
    }
    
    util.LogInfo("[ADMIN] detected newer version %s > current %s, exiting for watchdog restart...",
        contents.Meta.Version, currentVersion)
    
    // 3. 直接退出，看门狗会拉起新版本
    go func() {
        time.Sleep(1 * time.Second)
        syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
    }()
}
```

**平台/架构匹配说明**：

热替换的平台/架构匹配使用**编译时标识符**（`main.Platform` 和 `main.Arch`），而不是运行时标识符（`runtime.GOOS` 和 `runtime.GOARCH`）。

**原因**：
- Windows 7 兼容版本使用特殊的编译器（go-legacy-win7），编译时通过 ldflags 设置 `-X main.Platform=windows7`
- 运行时 `runtime.GOOS` 返回的是 `"windows"`，无法区分普通 Windows 版本和 Windows 7 兼容版本
- 如果使用 `runtime.GOOS` 比较，Windows 7 包（`platform: "windows7"`）永远无法匹配到运行中的 Windows 进程（`runtime.GOOS: "windows"`），导致热替换失效

**实现方式**：
```go
// main.go
var (
    Version  = "dev"
    Platform = "" // 编译时通过 ldflags 设置，例如 "linux", "windows", "windows7", "darwin"
    Arch     = "" // 编译时通过 ldflags 设置，例如 "amd64", "arm64"
)

// admin/admin.go
type AdminServer struct {
    // ...
    GetCurrentVersion func() string  // 返回当前版本
    GetPlatform       func() string  // 返回编译时平台标识符
    GetArch           func() string  // 返回编译时架构标识符
}

// main.go 组装
adminSrv.GetCurrentVersion = func() string { return Version }
adminSrv.GetPlatform = func() string { return Platform }
adminSrv.GetArch = func() string { return Arch }
```

**编译示例**：
```bash
# 普通 Windows 版本
go build -ldflags "-X main.Platform=windows -X main.Arch=amd64" -o phaethon.exe

# Windows 7 兼容版本
go build -ldflags "-X main.Platform=windows7 -X main.Arch=amd64" -o phaethon.exe

# Linux 版本
go build -ldflags "-X main.Platform=linux -X main.Arch=amd64" -o phaethon
```

**看门狗逻辑**（固化，不升级）：
```go
// 看门狗启动时：
// 1. 查找 .phaethon/packages/ 中最高版本的 pkg 文件
// 2. 检查 .phaethon/worker/ 是否已有对应版本的 binary
// 3. 如果没有，从 pkg 文件中提取 binary 到 .phaethon/worker/ 目录
// 4. 启动该 binary，工作目录设置为看门狗的工作目录
// 5. 监控 worker 心跳，如果崩溃或退出，重复 1-4

// 看门狗不升级的原因：
// - 逻辑简单：查找、提取、启动、监控
// - 不包含业务逻辑
// - 类似 BIOS/bootloader，可以固化
// - 即使 pkg 格式变化，可以做向后兼容
```

**调用时机**：
```go
// admin/package.go - apiPackageUpload
func (s *AdminServer) apiPackageUpload(w http.ResponseWriter, r *http.Request) {
    // ... 验证、保存 pkg 到工作目录 ...
    
    // 检查是否有更高版本
    go s.checkForNewerVersion(contents)
}

// admin/package.go - syncFromPeer
func (s *AdminServer) syncFromPeer(nodeID string) {
    // ... 下载、保存 pkg 到工作目录 ...
    
    // 每个包下载成功后检查是否有更高版本
    go s.checkForNewerVersion(contents)
}
```

**版本获取**：
```go
// admin/admin.go
type AdminServer struct {
    // ...
    // GetCurrentVersion returns the current running version. Set by main package.
    GetCurrentVersion func() string
    // GetPlatform returns the compile-time platform identifier. Set by main package.
    GetPlatform func() string
    // GetArch returns the compile-time architecture identifier. Set by main package.
    GetArch func() string
}

// main.go
adminSrv.GetCurrentVersion = func() string {
    return Version
}
adminSrv.GetPlatform = func() string {
    return Platform
}
adminSrv.GetArch = func() string {
    return Arch
}
```

**版本号升级规范**：
遵循语义化版本（Semantic Versioning）规范：
- **大版本号（MAJOR）**：不兼容的 API 变更或重大架构调整
  - 例如：v1.0.0 → v2.0.0（协议不兼容、配置格式大改）
- **小版本号（MINOR）**：向下兼容的功能新增
  - 例如：v0.2.0 → v0.3.0（新增热替换功能、新增 API 端点）
- **修订版本号（PATCH）**：向下兼容的问题修正
  - 例如：v0.3.0 → v0.3.1（修复 bug、性能优化）

**当前项目阶段**：
- 项目处于 v0.x.x 阶段，API 和架构仍在快速迭代
- 大版本号保持为 0，直到 API 稳定后升到 v1.0.0
- 新功能升小版本，bug 修复升修订版本

**构建和打包流程**：

1. **打 git tag**（触发版本号）：
   ```bash
   git tag v0.3.2
   git push origin v0.3.2
   ```

2. **构建 binary**（使用 Makefile）：
   ```bash
   make linux  # 构建 Linux amd64
   # 或
   make windows  # 构建 Windows amd64
   ```
   Makefile 会自动从 git tag 获取版本号。

3. **打包成 pkg**（使用 scripts/build-pkg.sh）：
   ```bash
   ./scripts/build-pkg.sh linux amd64
   ```
   脚本会：
   - 构建 binary（如果不存在）
   - 创建 meta.json
   - 使用 PHAETHON_SIGNING_KEY 签名（如果设置）
   - 打包成 .pkg 文件

4. **上传到 admin**：
   - 通过 admin UI 上传
   - 或通过 API：`curl -k -X POST -F 'file=@phaethon_linux_amd64_v0.3.2.pkg' https://localhost:39999/api/packages/upload`

5. **触发热替换**：
   - 上传成功后，如果平台/架构匹配且版本更高，自动触发热替换
   - 进程退出，看门狗拉起新版本
```

**注意事项**：
- watchdog 必须已部署，否则进程退出后不会自动拉起
- Windows 环境不支持原子替换（文件被占用），需要特殊处理或禁用
- 热替换失败不影响当前运行（回滚到旧版本）

### 6.9 路由注册

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

### 6.10 组装

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

### 6.11 Mesh 数据帧传输设计（drop-tail + 大缓冲 + 控制/数据分队列）

**设计定位**：mesh 层是网络层（IP 语义）——尽力转发、缓冲满即尾部丢弃，**不做背压**；可靠性由 overlay TCP 的重传/拥塞控制承担。中间路由节点必须保持响应性，不能因为某一流量扛不住而阻塞（head-of-line blocking 会拖垮包括 gossip/心跳在内的所有过路流量）。

**丢弃是安全的**：被丢的 mesh 帧 = 丢的 TCP segment。接收端 netstack 看到序号空洞 → 重复 ACK → 发送端 fast retransmit，流不损坏、自动恢复。mesh 层不重传。

**带宽估算**（帧 payload ≈ 1400B）：

| 传输路径 | 有效带宽 | RTT | BDP（带宽×时延积） |
|----------|----------|-----|--------------------|
| LAN 直连（VM↔QG） | 100M~1Gbps | 1~5ms | 12~600KB ≈ 9~430 帧 |
| trojan 经 GG（VPS 公网） | 10~100Mbps | 30~50ms | 37~625KB ≈ 27~450 帧 |
| h_tunnel 经 JF（移动网络） | 2~8Mbps | 80~150ms | 20~150KB ≈ 15~110 帧 |

BDP 只需几百帧，"吞吐匹配"不是瓶颈。真正的缓冲需求来自：

1. **源端突发**：overlay TCP 慢启动可短时以 LAN 速率灌帧（100Mbps ≈ 9000 帧/s），而 h_tunnel 有请求/响应循环的秒级停顿（协议受限，非带宽受限）。需吸收：突发速率 × 停顿时长（100Mbps × 1~2s ≈ 9000~18000 帧）
2. **整包突发**：单次 pkg 分发最大 ~30MB ≈ 21400 帧，理想情况下整包零丢帧

**参数**：

| 队列 | 容量 | 说明 |
|------|------|------|
| 数据队列 `writeCh`（FrameMeshPacket） | **16384 帧 ≈ 23MB/peer** | 覆盖 ~1.8s 的 100Mbps 突发停顿；22MB 以下整包零丢帧；更大的包丢帧由 overlay TCP 重传恢复 |
| 控制队列 `controlCh`（心跳/hello/gossip） | 512 帧 ≈ 0.7MB | **优先发送**（peerWriteLoop 先排空控制队列），防止被 23MB 数据排队拖死导致拓扑失效/误判掉线 |
| 入站 `meshInboundCh` | **16384 帧** | 中转节点入站吸收突发 |

**问题修正（2026-09-23 排查包分发大 body 失败）**：

| 问题 | 修正 |
|------|------|
| `writeCh` 仅 1024 帧（≈1.4MB），7.9MB 包体传输必然溢出丢帧 | 扩大到 16384 帧，并与控制帧分队列 |
| 控制帧（心跳/hello/gossip）与数据帧同队列，大流量时排队延迟可达分钟级 | 控制帧独立小队列 + 优先发送 |
| `meshInboundCh` 4096 帧，中转节点高吞吐下入站丢帧 | 扩大到 16384 帧 |
| `peerWriteLoop` 每帧 5s 写超时 → 拥塞时连接被踢（RST），overlay TCP 来不及重传，流损坏（表现为接收端 "read body failed"） | `FrameMeshPacket` 写超时放宽到 30s；控制帧保持 5s 快速失败 |
| 分发客户端 30s 超时对慢链路过紧（8MB 包在 2~8Mbps h_tunnel 上需 8~32s） | mesh HTTP 客户端超时放宽到 120s |
| admin 服务端 `ReadTimeout: 15s` 覆盖整个请求（含 body），慢链路上 7.9MB 包体传输超 15s 即被服务端掐断（接收端报 "read body failed" 400，实测 QG→JF 推送 15s 整失败） | 服务端 ReadTimeout 放宽到 120s，与 WriteTimeout 对齐 |
| 120s 仍不够：QG 同时向 3 个 peer 并发推 7.9MB，上行被分摊后 gg/jf 链路 <66KB/s，120s 整客户端超时（"awaiting headers" context deadline） | 客户端 Timeout 与服务端 Read/Write 统一放宽到 300s |

**不做的事**：
- 不对数据帧做阻塞式背压（中间路由不能被压）
- 不在 mesh 层做 ACK/重传（overlay TCP 已承担）

### 6.12 路由/域名副本选择与 via hop 标注（2026-09-23 排查 10.161.88.0/24 幽灵路由）

**问题现象**：GG 控制台看到 10.161.88.0/24 由 vm 通告（via [{ms9,1},{vm,1}]），VM 控制台该路由却经由 qg 而非 gg；经 mesh 访问 10.161.88.10/.12/.13 全部黑洞，.9 正常（哈希分流落点不同）。

**根因（两个缺陷叠加成自持环）**：

1. `broadcastGossip` 的 bestRoutes/bestDS 对同一前缀/域名的多条副本**先到先得**（`if _, ok := ...; !ok` 才收录），不比较路径长短。GG 上 vm 转发的副本（peer 注册顺序先于 ms9）被选为最优 → split horizon 禁止 GG 回发 vm → VM 只能从 qg 学到 → VM 又按规则转发回 GG → **GG→qg→vm→GG 三元环每 gossip 周期自我刷新**，形成"幽灵路由"（ms9 停止通告也不会消失；routes 无 hop 字段、无老化，claimedSubnets 的两道防残留机制对它不生效）。
2. `recomputeRoutes` 给 via 项标注的 hop 查的是**归属者**的全局最小 hop（`nodeIDToHop[r.NodeID]`），不是经该 peer 的真实路径 → GG 认为 vm(1) 与 ms9(1) 等价 → 转发按目的地址哈希二选一 → 分到 vm 的目的地址进 GG→vm→qg→gg→GG 转发环，TTL 耗尽丢弃。

**修正原则**：路由/域名路由本来就锚定 nodeID，最终解析全走 claimedSubnets——因此不给 routes 另搞老化机制，而是让**副本选择与 hop 标注统一从 claims 取真实距离**：

> 候选（转发者 p，归属者 X）的分数 = **p 的 ClaimedSubnets 中 X 项的 hop**（即经 p 到 X 的真实距离）；取最小者；p 的 claims 查不到 X → 该副本无效，丢弃。

| 位置 | 修正 |
|------|------|
| `broadcastGossip` bestRoutes | 先到先得 → 按上述分数取最小；无 claim 支撑的副本不收录 |
| `broadcastGossip` bestDS | 同上（域名路由同病同修） |
| `recomputeRoutes` via hop | 全局 `nodeIDToHop[X]` → 同一分数；无 claim 支撑的副本不进路由表 |
| `topology.go UpdateGossip` | 移除 claims hop≤20 过滤（冗余：邻居互认校验已在一两个周期内清场离线节点的声明，claims 的 min-hop 比较防膨胀） |

**效果**：GG 的 via 只剩 [ms9(1)]，环消散；VM 显示经由 gg(2)、转发走 gg；ms9 离线后 claims 互认清场 → 路由候选全部失去支撑 → 路由自然消失，无残留。协议格式与 P2PProtocolVersion 不变。

---

## 七、文件变更清单

| 文件 | 变更 |
|------|------|
| `mesh/topology.go` | GossipInfo 加 Cmd、ProtocolVersion 字段 |
| `p2p/p2p.go` | 删除 HelloMsg，用 GossipInfo 替代；handleHello 改为提取 nodeID + UnregisterPeer → RegisterPeer + 处理拓扑；handleGossip 处理拓扑；processGossipInfo 共用逻辑；sendHello/sendGossip 构建 GossipInfo；ProtocolVersion → 6；新增 ResendHelloToAll |
| `mesh/mesh.go` | 新增 BuildGossipInfo()、UnregisterPeerByNodeID()、OnPeerRegistered 回调、CloseCh()；冲突后调 ResendHelloToAll；gossipLoop 中 RegisterPeer 后调 broadcastGossip |
| `admin/admin.go` | 新增 PeerBrief、meshHTTPClient、peerLister、meshDialFn、adminPort；新增 SetPeerLister、SetMeshDialFn、SetAdminPort；新增 GetCurrentVersion 回调获取当前版本；更新 isPublicPath 支持 method 参数；包端点无需认证 |
| `admin/package.go` | 新增 apiPackageReceive；apiPackagePublish 后调 DistributePackage；SyncFromPeer 实现同步逻辑（目标集合、下载、retention）；版本单调递增检查；新增 tryHotSwap 实现热替换（平台/架构匹配 + 版本更高时触发）；三个触发点：apiPackageUpload、apiPackageReceive、syncFromPeer |
| `main.go` | 组装：SetPeerLister、SetMeshDialFn、SetAdminPort、GetCurrentVersion 回调、OnPeerRegistered 回调、5 分钟定时同步 |
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
| mesh 帧传输 | **drop-tail + 大缓冲** | mesh 层是 IP 语义，中间路由不被压；丢帧由 overlay TCP 重传恢复 |
| 同步删除 | **peer 与本地 merge 后取 top N** | 防止空列表 peer 误删本地包 |
| 分发寻址 | **域名不带端口** | netstack localNodeDomain 直连 admin handler，端口无关 |
