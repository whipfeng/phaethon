# Mesh 双队列并发设计

**创建时间**: 2026-10-28  
**状态**: 设计中  
**优先级**: 高

## 问题背景

### 当前问题
1. **Topology PeerInfo 累积**：P2P 连接快速重连时，eventCh 满了导致 unregister 事件被丢弃，PeerInfo 条目无法清理，拓扑显示多条连线
2. **eventCh 全局队列瓶颈**：所有 mesh 事件（register/unregister/gossip/config/tick）都走同一个队列，互相阻塞
3. **性能隔离不足**：gossip 广播慢会阻塞关键的 register/unregister 操作

### 根本原因
- eventCh 使用非阻塞发送，满了就丢弃事件
- 单队列设计导致关键操作和非关键操作互相影响
- 没有区分事件的优先级和重要性

## 设计方案

### 双队列架构

```
生产者
  │
  ├─ register/unregister ──► criticalCh (阻塞) ──► criticalWorker ──► 修改 topology
  │                                                                    重算路由
  ├─ gossip ───────────────► normalCh (非阻塞) ──► gossipWorker ────► 读取快照
                                                                      广播 gossip
                                                                      ↓
                                                                  转发到 criticalCh
  └─ config/tick ──────────► normalCh

数据:
  topology.peers:  无锁 (单 worker 改)
  routeTable:      atomic.Value (无锁读)
  topologySnapshot: atomic.Value (无锁读)
```

### 队列职责分离

| 队列 | 类型 | 容量 | 事件类型 | 满了怎么办 | 原因 |
|------|------|------|----------|-----------|------|
| `criticalCh` | 阻塞 | 256 | peerRegister, peerUnregister, processGossip, recomputeRoute | **阻塞等待** | 丢了会导致 PeerInfo 累积或路由错误 |
| `normalCh` | 非阻塞 | 1024 | gossipBroadcast, configUpdate, tick | **丢弃** | 定时器会补上，不影响核心功能 |

### Worker 职责分离

| Worker | 职责 | 修改的数据 | 读取的数据 |
|--------|------|-----------|-----------|
| `criticalWorker` | 修改 topology + 重算路由 | topology.peers, routeTable | - |
| `gossipWorker` | 读取 topology + 广播 gossip | - | topologySnapshot, routeTable |

### 并发控制策略

| 数据 | criticalWorker | gossipWorker | 并发控制 |
|------|----------------|--------------|---------|
| `topology.peers` | 写（增删） | 读（构建 gossip） | `RWMutex` |
| `routeTable` | 写（原子替换） | 读（无锁） | `atomic.Value` |
| `domainTrie` | 写（原子替换） | 读（无锁） | `atomic.Value` |
| `topologySnapshot` | 写（原子替换） | 读（无锁） | `atomic.Value` |

## 实现细节

### 1. 数据结构

```go
type MeshManager struct {
    // ... existing fields ...
    
    // Dual-channel design
    criticalCh chan meshEvent  // blocking queue for critical ops
    normalCh   chan meshEvent  // non-blocking queue for non-critical ops
    
    // Topology snapshot for lock-free reads by gossipWorker
    topologySnapshot atomic.Value  // stores *TopologySnapshot
}

type meshEventKind int

const (
    // Critical events (criticalCh)
    peerRegister meshEventKind = iota
    peerUnregister
    processGossip
    recomputeRoute
    
    // Normal events (normalCh)
    gossipBroadcast
    configUpdate
    tick
)

type TopologySnapshot struct {
    peers []*PeerInfo  // copy of topology.peers
}
```

### 2. 事件发送

```go
// Critical operations: blocking send
func (m *MeshManager) RegisterPeer(sender PeerSender) {
    m.criticalCh <- meshEvent{kind: peerRegister, sender: sender}
}

func (m *MeshManager) UnregisterPeer(sender PeerSender) {
    m.criticalCh <- meshEvent{kind: peerUnregister, sender: sender}
}

// Non-critical operations: non-blocking send
func (m *MeshManager) UpdateConfig(domainSuffixes, advertise []string) {
    m.mu.Lock()
    m.domainSuffixes = domainSuffixes
    m.advertise = advertise
    m.mu.Unlock()
    select {
    case m.normalCh <- meshEvent{kind: configUpdate}:
    default:
        util.LogModuleWarn("mesh", "normalCh full, dropping config update")
    }
}

func (m *MeshManager) TriggerGossip() {
    select {
    case m.normalCh <- meshEvent{kind: tick}:
    default:
        util.LogModuleWarn("mesh", "normalCh full, dropping tick")
    }
}
```

### 3. Worker 实现

```go
// criticalWorker: serial processing of critical operations
func (m *MeshManager) criticalWorker() {
    for {
        select {
        case <-m.closeCh:
            return
        case ev := <-m.criticalCh:
            switch ev.kind {
            case peerRegister:
                m.topology.mu.Lock()
                m.topology.peers = append(m.topology.peers, &PeerInfo{
                    Sender:   ev.sender,
                    LastSeen: time.Now(),
                })
                m.topology.mu.Unlock()
                m.updateTopologySnapshot()
                m.recomputeRoutes()
                m.qualityTracker.RemoveNode(ev.sender.GetNodeID())
                util.LogModuleInfo("mesh", "[MESH] peer registered: %s", ev.sender.GetNodeID())
                if m.OnPeerRegistered != nil {
                    go m.OnPeerRegistered(ev.sender.GetNodeID())
                }
                // Trigger gossip broadcast (non-critical)
                select {
                case m.normalCh <- meshEvent{kind: gossipBroadcast}:
                default:
                }
                
            case peerUnregister:
                nodeID := ev.sender.GetNodeID()
                m.topology.mu.Lock()
                m.topology.UnregisterPeer(ev.sender)
                m.topology.mu.Unlock()
                m.updateTopologySnapshot()
                m.recomputeRoutes()
                util.LogModuleInfo("mesh", "[MESH] peer unregistered: %s", nodeID)
                
            case processGossip:
                m.topology.mu.Lock()
                m.topology.UpdateGossip(ev.sender, ev.info)
                m.topology.mu.Unlock()
                m.updateTopologySnapshot()
                m.recomputeRoutes()
                
            case recomputeRoute:
                m.recomputeRoutes()
            }
        }
    }
}

// gossipWorker: serial processing of non-critical operations
func (m *MeshManager) gossipWorker() {
    ticker := time.NewTicker(15 * time.Second)
    defer ticker.Stop()
    
    for {
        select {
        case <-m.closeCh:
            return
        case <-ticker.C:
            m.broadcastGossipFromSnapshot()
        case ev := <-m.normalCh:
            switch ev.kind {
            case gossipBroadcast:
                m.broadcastGossipFromSnapshot()
            case configUpdate:
                m.recomputeRoutes()
                m.broadcastGossipFromSnapshot()
                db.BumpRuntimeVersion("mesh")
            }
        }
    }
}
```

### 4. 快照更新

```go
func (m *MeshManager) updateTopologySnapshot() {
    m.topology.mu.RLock()
    peersCopy := make([]*PeerInfo, len(m.topology.peers))
    copy(peersCopy, m.topology.peers)
    m.topology.mu.RUnlock()
    
    snapshot := &TopologySnapshot{peers: peersCopy}
    m.topologySnapshot.Store(snapshot)
}

func (m *MeshManager) broadcastGossipFromSnapshot() {
    snapshot := m.topologySnapshot.Load().(*TopologySnapshot)
    if snapshot == nil || len(snapshot.peers) == 0 {
        return
    }
    // Build gossip data from snapshot
    data := m.buildGossipData(snapshot.peers)
    m.p2p.BroadcastMeshGossip(data)
}
```

### 5. 启动 Worker

```go
func (m *MeshManager) Start() {
    go m.criticalWorker()
    go m.gossipWorker()
}
```

## 优势

1. **关键操作不丢失**：criticalCh 阻塞，保证 register/unregister 执行
2. **性能隔离**：gossip 广播慢不会阻塞关键操作
3. **最小锁竞争**：
   - `topology.peers` 用 `RWMutex`（写少读多）
   - `routeTable` 用 `atomic.Value`（无锁读取）
4. **清晰的职责分离**：关键操作 vs 通知操作

## 迁移计划

1. 添加 criticalCh 和 normalCh 字段
2. 修改 RegisterPeer/UnregisterPeer 发送到 criticalCh
3. 修改 gossip/configUpdate/tick 发送到 normalCh
4. 实现 criticalWorker 和 gossipWorker
5. 移除 gossipLoop
6. 添加 topologySnapshot 原子值
7. 测试验证

## 风险

1. **criticalCh 阻塞**：如果 criticalWorker 处理慢，可能导致 RegisterPeer 调用方阻塞
   - 缓解：criticalWorker 快速处理，复杂操作异步化
2. **快照一致性**：gossipWorker 读取的快照可能稍微过时
   - 缓解：最终一致性，不影响核心功能

## 测试计划

1. 单元测试：验证双队列事件处理
2. 集成测试：模拟 P2P 快速重连，验证 PeerInfo 不累积
3. 性能测试：验证 gossip 广播不影响 register/unregister
4. 压力测试：验证 criticalCh 不会长时间阻塞
