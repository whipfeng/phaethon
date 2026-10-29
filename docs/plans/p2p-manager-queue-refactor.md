# P2P Manager 队列化重构设计

**创建时间**: 2026-10-28  
**状态**: 设计中  
**优先级**: 高

## 背景

当前 P2P Manager 使用 `sync.RWMutex` 保护 `m.peers` 和 `m.proxyPeers` 两个 map，存在以下问题：

1. **竞态条件复杂**：需要双重检查（dial 前和 dial 后）防止并发创建
2. **清理顺序混乱**：sessionLoop 需要在调用 StartPeer 前手动清理 maps
3. **逻辑分散**：map 操作散落在多个函数中，难以理解和维护
4. **死锁风险**：锁的持有时间长，嵌套调用可能导致死锁

## 设计目标

1. **无锁化**：所有 map 操作通过 command queue 串行化，无需锁
2. **逻辑集中**：所有 map 修改都在 worker goroutine 中，易于理解和调试
3. **无竞态**：命令按顺序处理，天然避免并发问题
4. **死锁安全**：分析所有调用路径，确保无死锁风险

## 核心设计

### 1. Command Queue

```go
type P2PManager struct {
    // Command queue for serialized map operations
    cmdCh chan peerCommand
    
    // Read-only snapshot for queries (updated by worker)
    peersSnapshot atomic.Value  // stores map[string]*Peer
    
    // ... other fields (no mutex needed)
}

type peerCommand struct {
    action    string          // "start", "stop", "snapshot"
    proxy     *config.Proxy   // for "start"
    proxyName string          // for "stop"
    respCh    chan interface{} // for "snapshot" (optional)
}
```

### 2. Worker Goroutine

Worker 是唯一访问和修改 maps 的 goroutine：

```go
func (m *P2PManager) worker() {
    peers := make(map[string]*Peer)
    proxyPeers := make(map[string]*Peer)
    
    for cmd := range m.cmdCh {
        switch cmd.action {
        case "start":
            m.handleStart(cmd, peers, proxyPeers)
        case "stop":
            m.handleStop(cmd, peers, proxyPeers)
        case "snapshot":
            m.handleSnapshot(cmd, proxyPeers)
        }
    }
}
```

### 3. 命令处理

#### Start 命令

```go
func (m *P2PManager) handleStart(cmd peerCommand, peers, proxyPeers map[string]*Peer) {
    // 1. 检查是否已存在
    if _, ok := proxyPeers[cmd.proxy.Name]; ok {
        util.LogModuleDebug("p2p", "[P2P] peer already exists for proxy %s, skipping", cmd.proxy.Name)
        return
    }
    
    // 2. 创建 peer 对象
    peer := m.createPeerObject(cmd.proxy)
    
    // 3. Dial 连接（在 worker 中完成，阻塞队列）
    transport, err := m.dialTransport(cmd.proxy)
    if err != nil {
        util.LogModuleWarn("p2p", "[P2P] dial failed for proxy %s: %v", cmd.proxy.Name, err)
        return
    }
    peer.transport = transport
    
    // 4. 添加到 maps
    peers[peer.ID] = peer
    proxyPeers[cmd.proxy.Name] = peer
    
    // 5. 更新快照
    m.updateSnapshot(proxyPeers)
    
    // 6. 启动 session（异步）
    go m.runSession(peer)
    
    // 7. 触发版本号通知（SSE）
    m.notifyStatusChange()
}
```

#### Stop 命令

```go
func (m *P2PManager) handleStop(cmd peerCommand, peers, proxyPeers map[string]*Peer) {
    peer, ok := proxyPeers[cmd.proxyName]
    if !ok {
        return
    }
    
    // 1. 从 maps 移除
    delete(peers, peer.ID)
    delete(proxyPeers, cmd.proxyName)
    
    // 2. 更新快照
    m.updateSnapshot(proxyPeers)
    
    // 3. 停止 peer
    stopPeer(peer)
    
    // 4. 触发版本号通知
    m.notifyStatusChange()
}
```

#### Snapshot 命令

```go
func (m *P2PManager) handleSnapshot(cmd peerCommand, proxyPeers map[string]*Peer) {
    if cmd.respCh != nil {
        // 返回快照副本
        snapshot := make(map[string]*Peer)
        for k, v := range proxyPeers {
            snapshot[k] = v
        }
        cmd.respCh <- snapshot
    }
}
```

### 4. 公共 API

```go
// StartPeer 发送 start 命令到队列，立即返回
func (m *P2PManager) StartPeer(proxy *config.Proxy) {
    m.cmdCh <- peerCommand{action: "start", proxy: proxy}
}

// StopPeerByProxyName 发送 stop 命令到队列
func (m *P2PManager) StopPeerByProxyName(proxyName string) {
    m.cmdCh <- peerCommand{action: "stop", proxyName: proxyName}
}

// GetPeers 读取快照（无锁）
func (m *P2PManager) GetPeers() []*Peer {
    snapshot := m.peersSnapshot.Load().(map[string]*Peer)
    result := make([]*Peer, 0, len(snapshot))
    for _, peer := range snapshot {
        result = append(result, peer)
    }
    return result
}

// GetPeerProxy 从快照中查找（无锁）
func (m *P2PManager) GetPeerProxy(proxyName string) *config.Proxy {
    snapshot := m.peersSnapshot.Load().(map[string]*Peer)
    if peer, ok := snapshot[proxyName]; ok {
        return peer.proxy
    }
    return nil
}
```

### 5. SessionLoop 清理

SessionLoop 不再需要手动清理 maps，只需发送 stop 命令：

```go
func (m *P2PManager) sessionLoop(peer *Peer) {
    // ... session logic ...
    
    // Session ended, send stop command to queue
    m.cmdCh <- peerCommand{action: "stop", proxyName: peer.proxy.Name}
    
    // Reconnect if needed
    if peer.GetStatus() != "stopped" {
        // ... backoff logic ...
        m.cmdCh <- peerCommand{action: "start", proxy: peer.proxy}
    }
}
```

## 调用方分析

### 1. main.go:428（启动时）

```go
for _, proxy := range proxies {
    go p2p.GlobalP2PManager.StartPeer(proxy)
}
```

**影响**：无。StartPeer 发送命令到队列，立即返回。

### 2. main.go:2149（Config Watcher）

```go
if shouldRun && !exists {
    go p2p.GlobalP2PManager.StartPeer(proxy)
}
```

**影响**：无。`!exists` 检查基于快照，可能不准确，但 worker 会再次检查。

### 3. p2p/p2p.go:1190（RestartPeer）

```go
func (m *P2PManager) RestartPeer(proxy *config.Proxy) {
    m.StopPeerByProxyName(proxy.Name)
    go m.StartPeer(proxy)
}
```

**影响**：Stop 和 Start 命令按顺序进入队列，worker 依次处理。无竞态。

### 4. p2p/p2p.go:1443（SessionLoop 重连）

```go
m.cmdCh <- peerCommand{action: "stop", proxyName: peer.proxy.Name}
m.cmdCh <- peerCommand{action: "start", proxy: peer.proxy}
```

**影响**：Stop 和 Start 命令按顺序进入队列。无竞态。

## 死锁风险分析

### 风险 1：队列满阻塞

**场景**：`cmdCh` 满了，发送方阻塞。

**分析**：
- `cmdCh` 容量设置为 1024（足够大）
- Worker 处理命令很快（除了 dial，但 dial 有超时）
- 正常情况下不会满

**缓解**：
- 使用带缓冲的 channel：`make(chan peerCommand, 1024)`
- 如果队列满，发送方阻塞等待（不会死锁，因为 worker 一直在处理）

### 风险 2：Worker 等待 Dial 完成

**场景**：Worker 在 handleStart 中 dial，阻塞队列处理。

**分析**：
- Dial 有超时（通常 10-30 秒）
- 其他命令（stop, snapshot）会被延迟
- 不会死锁，只是延迟

**缓解**：
- 可以接受，因为 P2P 连接数不多
- 如果需要，可以把 dial 移到单独的 goroutine，但会增加复杂度

### 风险 3：Snapshot 请求阻塞

**场景**：handleSnapshot 发送响应到 respCh，但接收方没在等待。

**分析**：
- respCh 是无缓冲 channel
- 如果接收方放弃等待，worker 会永久阻塞

**缓解**：
- 使用 select + timeout：
  ```go
  select {
  case cmd.respCh <- snapshot:
  case <-time.After(5 * time.Second):
      util.LogModuleWarn("p2p", "[P2P] snapshot response timeout")
  }
  ```
- 或者 respCh 使用带缓冲 channel：`make(chan interface{}, 1)`

### 风险 4：StopPeer 在 StartPeer 中调用

**场景**：handleStart 发现已存在，调用 stopPeer。

**分析**：
- 当前设计：handleStart 发现已存在，直接 return，不停止旧的
- 如果需要停止旧的，应该发送 stop 命令到队列，而不是直接调用

**缓解**：
- 不在 handleStart 中调用 stopPeer
- 如果需要替换，先发送 stop，再发送 start

## 实现步骤

### 阶段 1：基础设施

1. 修改 P2PManager 结构体：
   - 添加 `cmdCh chan peerCommand`
   - 添加 `peersSnapshot atomic.Value`
   - 移除 `mu sync.RWMutex`（或保留用于其他用途）

2. 实现 worker goroutine：
   - 维护 peers 和 proxyPeers maps
   - 处理 start, stop, snapshot 命令
   - 更新 peersSnapshot

3. 实现命令处理函数：
   - handleStart
   - handleStop
   - handleSnapshot

### 阶段 2：API 迁移

1. 修改 StartPeer：发送 start 命令到队列
2. 修改 StopPeerByProxyName：发送 stop 命令到队列
3. 修改 GetPeers/GetPeerProxy：读取快照
4. 修改 RestartPeer：发送 stop + start 命令

### 阶段 3：SessionLoop 迁移

1. 修改 sessionLoop：发送 stop 命令到队列（不再手动清理 maps）
2. 修改重连逻辑：发送 start 命令到队列

### 阶段 4：清理和测试

1. 移除不再需要的锁和双重检查逻辑
2. 编译验证
3. 部署到测试环境
4. 验证功能：
   - 启动时创建 peers
   - Config 变更触发 restart
   - 断线重连
   - Admin API 查询
   - SSE 通知

## 性能影响

### 正面影响

1. **无锁**：读取快照不需要锁，性能更好
2. **减少竞态**：命令串行化，避免重复 dial

### 负面影响

1. **Dial 阻塞队列**：worker 在 dial 时，其他命令会被延迟
   - 影响：P2P 连接建立变慢
   - 缓解：P2P 连接数不多，可接受
2. **Channel 开销**：每次操作都需要发送命令到 channel
   - 影响：微小，可忽略

## 总结

队列化设计使 P2P Manager 的逻辑更清晰、更安全：

- ✅ 无锁化：所有 map 操作通过 queue 串行化
- ✅ 无竞态：命令按顺序处理，天然避免并发问题
- ✅ 易理解：所有 map 修改都在 worker 中，逻辑集中
- ✅ 死锁安全：分析了所有风险点，都有缓解措施

代价是 dial 会阻塞队列，但 P2P 连接数不多，可接受。
