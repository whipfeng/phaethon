# 规则感知 P2P 路由设计

> **状态：暂时搁置** — 待进一步考虑是否实现此功能

## 背景

当前 TUN 引擎在匹配到代理规则后，总是通过代理服务器建立连接。但对于已建立 P2P 连接的代理（mesh 节点），可以直接通过 P2P 路径发送，减少延迟和代理服务器负载。

## 目标

1. **规则匹配后检查 P2P**：Match 到 proxy 后，检查该 proxy 是否有活跃的 P2P 连接
2. **P2P 优先**：如果有 P2P 连接，直接通过 mesh 发送到目标节点
3. **Fallback 降级**：如果没有 P2P，走原来的代理逻辑
4. **连接缓存表**：类似 NAT 表，记录路由决策，支持快速路径
5. **规则变化清理**：规则配置变化时清理缓存

## 核心设计

### 1. P2P 路由缓存表（类似 NAT 表）

```go
// tun/route_cache.go

type RouteCacheKey struct {
    SrcIP    string
    SrcPort  int
    DstIP    string
    DstPort  int
    Protocol string  // "tcp" or "udp"
}

type RouteCacheEntry struct {
    // 路由决策
    ProxyName  string  // 匹配到的代理名称
    TargetPeer string  // P2P 目标节点 ID（如果走 P2P）
    UseP2P     bool    // 是否走 P2P 路径
    
    // 生命周期（类似 NAT）
    CreatedAt time.Time
    LastUsed  time.Time
    Timeout   time.Duration  // 默认 30 秒
}

type RouteCache struct {
    mu      sync.RWMutex
    entries map[RouteCacheKey]*RouteCacheEntry
    
    // 清理
    cleanupInterval time.Duration  // 默认 10 秒
    stopCh          chan struct{}
}

func NewRouteCache() *RouteCache {
    c := &RouteCache{
        entries:         make(map[RouteCacheKey]*RouteCacheEntry),
        cleanupInterval: 10 * time.Second,
        stopCh:          make(chan struct{}),
    }
    go c.cleanupLoop()
    return c
}

// Lookup 查缓存，命中时延长到期时间
func (c *RouteCache) Lookup(key RouteCacheKey) (*RouteCacheEntry, bool) {
    c.mu.RLock()
    entry, ok := c.entries[key]
    c.mu.RUnlock()
    
    if !ok {
        return nil, false
    }
    
    // 延长到期时间
    c.mu.Lock()
    entry.LastUsed = time.Now()
    c.mu.Unlock()
    
    return entry, true
}

// Store 记录路由决策
func (c *RouteCache) Store(key RouteCacheKey, proxyName, targetPeer string, useP2P bool) {
    c.mu.Lock()
    defer c.mu.Unlock()
    
    c.entries[key] = &RouteCacheEntry{
        ProxyName:  proxyName,
        TargetPeer: targetPeer,
        UseP2P:     useP2P,
        CreatedAt:  time.Now(),
        LastUsed:   time.Now(),
        Timeout:    30 * time.Second,
    }
}

// Remove 删除条目（连接关闭时）
func (c *RouteCache) Remove(key RouteCacheKey) {
    c.mu.Lock()
    defer c.mu.Unlock()
    delete(c.entries, key)
}

// Clear 清理所有缓存（规则变化时调用）
func (c *RouteCache) Clear() {
    c.mu.Lock()
    defer c.mu.Unlock()
    c.entries = make(map[RouteCacheKey]*RouteCacheEntry)
}

// 后台清理：删除超时条目
func (c *RouteCache) cleanupLoop() {
    ticker := time.NewTicker(c.cleanupInterval)
    defer ticker.Stop()
    
    for {
        select {
        case <-c.stopCh:
            return
        case <-ticker.C:
            c.cleanup()
        }
    }
}

func (c *RouteCache) cleanup() {
    c.mu.Lock()
    defer c.mu.Unlock()
    
    now := time.Now()
    for key, entry := range c.entries {
        if now.Sub(entry.LastUsed) > entry.Timeout {
            delete(c.entries, key)
        }
    }
}
```

### 2. 集成到 writeLoop（IP 包转发）

```go
// tun/engine.go - writeLoop

func (e *Engine) writeLoop() {
    for {
        pkt := e.linkEP.ReadContext(ctx)
        data := pkt.ToBuffer().Flatten()
        
        // 提取五元组 + TCP 标志
        srcIP, dstIP, srcPort, dstPort, proto, tcpFlags := extractFiveTuple(data)
        key := RouteCacheKey{SrcIP: srcIP, SrcPort: srcPort, DstIP: dstIP, DstPort: dstPort, Protocol: proto}
        
        // TCP 连接结束：清理缓存
        if proto == "tcp" && (tcpFlags&FIN != 0 || tcpFlags&RST != 0) {
            e.routeCache.Remove(key)
            pkt.DecRef()
            continue
        }
        
        // 1. 查缓存
        if entry, ok := e.routeCache.Lookup(key); ok {
            // 缓存命中：使用缓存的路由决策
            if entry.UseP2P {
                // 尝试通过 mesh 发给目标节点
                err := e.sendToMeshPeer(entry.TargetPeer, pkt)
                if err == nil {
                    pkt.DecRef()
                    continue  // 成功
                }
                // P2P 发送失败（链路断开）→ 删除缓存，fallback
                e.routeCache.Remove(key)
            } else {
                // 走原来的代理路径
                err := e.sendViaProxy(entry.ProxyName, pkt)
                if err == nil {
                    pkt.DecRef()
                    continue
                }
                // 代理发送失败 → 删除缓存
                e.routeCache.Remove(key)
            }
        }
        
        // 2. 缓存未命中（或发送失败）：匹配规则
        proxy := e.matchRule(dstIP)
        
        // 3. 检查 P2P
        useP2P, targetPeer := e.checkP2P(proxy)
        
        // 4. 记录缓存（TCP SYN 或 UDP 首包）
        e.routeCache.Store(key, proxy.Name, targetPeer, useP2P)
        
        // 5. 路由
        if useP2P {
            err := e.sendToMeshPeer(targetPeer, pkt)
            if err != nil {
                // P2P 失败，降级到代理
                e.routeCache.Store(key, proxy.Name, "", false)
                e.sendViaProxy(proxy.Name, pkt)
            }
        } else {
            e.sendViaProxy(proxy.Name, pkt)
        }
        
        pkt.DecRef()
    }
}

// extractFiveTuple 从 IP 包提取五元组 + TCP 标志
func extractFiveTuple(data []byte) (srcIP, dstIP string, srcPort, dstPort int, proto string, tcpFlags byte) {
    // 解析 IP 头
    if len(data) < 20 {
        return
    }
    srcIP = net.IP(data[12:16]).String()
    dstIP = net.IP(data[16:20]).String()
    protoNum := data[9]
    
    // 解析 TCP/UDP 头
    ihl := int(data[0]&0x0f) * 4
    if len(data) < ihl+4 {
        return
    }
    srcPort = int(data[ihl])<<8 | int(data[ihl+1])
    dstPort = int(data[ihl+2])<<8 | int(data[ihl+3])
    
    switch protoNum {
    case 6:  // TCP
        proto = "tcp"
        if len(data) >= ihl+14 {
            tcpFlags = data[ihl+13]  // TCP flags at offset 13
        }
    case 17: // UDP
        proto = "udp"
    default:
        proto = fmt.Sprintf("%d", protoNum)
    }
    
    return
}

// TCP 标志常量
const (
    FIN = 0x01
    SYN = 0x02
    RST = 0x04
    PSH = 0x08
    ACK = 0x10
)
```

### 3. 规则变化时清理缓存

```go
// tun/engine.go

// OnRulesChanged 规则配置变化时调用（配置重载）
func (e *Engine) OnRulesChanged() {
    e.routeCache.Clear()
}
```

### 4. 组装

```go
// main.go

// 创建路由缓存
routeCache := tun.NewRouteCache()

// 注入到 TUN 引擎
tunEngine.SetRouteCache(routeCache)

// 规则变化时清理缓存
config.OnReload(func() {
    tunEngine.OnRulesChanged()
})
```

## 优势

1. **性能优化**：P2P 直连减少代理服务器负载和延迟
2. **智能降级**：P2P 失败自动 fallback 到代理路径
3. **连接稳定**：五元组缓存确保 TCP/UDP 连接生命周期内路径不变
4. **生命周期管理**：
   - TCP：SYN 创建缓存，FIN/RST 清理缓存
   - UDP：首包创建缓存，超时清理
   - 活动续期：有数据传输时延长到期时间
5. **规则感知**：规则变化时自动清理缓存
6. **透明性**：对上层应用透明，无需修改配置

## 实现步骤

1. 实现 `RouteCache`（缓存表 + 活动续期 + 超时清理）
2. 实现 `checkP2P`（检查代理是否有 P2P 连接）
3. 实现 `sendToMeshPeer`（通过 mesh 发送 IP 包到目标节点）
4. 集成到 `writeLoop`（IP 包转发时查缓存 + 匹配规则）
5. 实现 `OnRulesChanged`（规则变化时清理缓存）
6. 测试验证

## 待决问题

1. **mesh 层如何转发 IP 包**：目标节点收到 IP 包后如何处理？是直接注入 netstack 还是其他方式？
2. **域名路径**：DNS 解析时是否也需要匹配规则 + 检查 P2P？
3. **监控和调试**：如何查看缓存表状态？需要 admin API

## 替代方案

如果实现复杂度过高，可以考虑：
- **不实现此功能**：继续使用静态路由配置
- **简化版**：只支持域名路径，不支持纯 IP 路径
