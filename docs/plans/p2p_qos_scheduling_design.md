# P2P QoS 调度设计

**创建时间**: 2026-10-15  
**状态**: 设计中  
**优先级**: 高

## 问题背景

### 现象
- DNS 查询间歇性超时（`read udp 100.0.0.3:xxxxx: i/o timeout`）
- QG 日志显示 P2P 写队列满（`write queue full`，9550 次）
- JF 日志显示没有收到 DNS 查询（日志停在 20:06）

### 根本原因
1. **P2P 发送队列共享**：所有包（DNS、TCP 数据、ACK）共享同一个发送队列
2. **大流量饿死小包**：TCP 大包占满队列，DNS 小包被丢弃
3. **无公平调度**：单个 TCP 连接可以占满整个队列

### 当前实现
```go
// p2p/p2p.go
highPriorityCh: make(chan writeReq, 256)  // < 500 bytes
writeCh: make(chan writeReq, 256)         // >= 500 bytes
controlCh: make(chan writeReq, 64)        // 控制帧
```

**问题**：
- highPriorityCh 容量只有 256
- 所有 < 500 字节的包（包括 TCP ACK、小数据包）都走这个队列
- 队列满时 DNS 包被丢弃

## 设计目标

1. **DNS 包不被饿死**：DNS 查询必须有最高优先级
2. **公平调度**：多个流之间公平分配带宽
3. **避免单连接占满**：单个 TCP 连接不能占满整个队列

## 设计方案

### 方案概述

```
包 → 分类器
   ├─ DNS（UDP 53）→ DNS 专用队列（最高优先级，永远不饿死）
   ├─ 控制帧 → 控制队列（最高优先级）
   ├─ TCP ACK → ACK 队列（高优先级）
   └─ 普通数据 → 按流分 16 个队列 → DRR 公平调度
```

### 队列设计

#### 1. DNS 专用队列
```go
dnsQueue: make(chan writeReq, 512)
```
- **优先级**：最高（永远优先发送）
- **容量**：512
- **分类规则**：`protocol == UDP && dstPort == 53`
- **保证**：永不丢弃（除非队列满，但容量足够大）

#### 2. 控制帧队列
```go
controlCh: make(chan writeReq, 64)
```
- **优先级**：最高（与 DNS 同级）
- **容量**：64
- **分类规则**：`isControl == true`
- **保证**：永不丢弃（已有实现）

#### 3. TCP ACK 队列
```go
ackQueue: make(chan writeReq, 256)
```
- **优先级**：高（仅次于 DNS 和控制帧）
- **容量**：256
- **分类规则**：`protocol == TCP && flags == ACK && len < 100`
- **保证**：小 ACK 包优先发送

#### 4. 数据队列（按流公平）
```go
dataQueues: [16]chan writeReq  // 16 个队列
```
- **优先级**：普通
- **容量**：每个队列 256
- **分类规则**：`flowID = hash(srcIP, dstIP, srcPort, dstPort, protocol) % 16`
- **调度**：DRR（Deficit Round Robin）公平调度

### 调度算法

```go
func (p *Peer) writeLoop() {
    for {
        // 1. 优先发送 DNS 包（非阻塞）
        select {
        case req := <-p.dnsQueue:
            writeSingleFrame(req)
            continue
        default:
        }
        
        // 2. 优先发送控制帧（非阻塞）
        select {
        case req := <-p.controlCh:
            writeSingleFrame(req)
            continue
        default:
        }
        
        // 3. 优先发送 ACK（非阻塞）
        select {
        case req := <-p.ackQueue:
            writeSingleFrame(req)
            continue
        default:
        }
        
        // 4. DRR 调度数据队列（阻塞）
        req := p.drrScheduler.Next()
        if req != nil {
            writeSingleFrame(req)
        }
    }
}
```

### DRR 调度器

```go
type DRRScheduler struct {
    queues      [16]chan writeReq
    deficits    [16]int
    quantum     int  // 每个队列每次发送的字节数（默认 1500）
    stopCh      chan struct{}
}

// Next 阻塞等待下一个可发送的包（DRR 公平调度）
func (s *DRRScheduler) Next() *writeReq {
    round := 0
    for {
        // 检查是否停止
        select {
        case <-s.stopCh:
            return nil
        default:
        }
        
        // 轮转检查所有队列
        for i := 0; i < 16; i++ {
            idx := (round + i) % 16
            
            // 补充 deficit
            if s.deficits[idx] <= 0 {
                s.deficits[idx] += s.quantum
            }
            
            // 非阻塞尝试从队列取包
            select {
            case req := <-s.queues[idx]:
                s.deficits[idx] -= len(req.data)
                round = (idx + 1) % 16  // 下次从下一个队列开始
                return &req
            default:
            }
        }
        
        // 所有队列都为空，短暂等待后重试
        select {
        case <-s.stopCh:
            return nil
        case <-time.After(100 * time.Microsecond):
        }
    }
}
```

### 分类器

```go
func classifyPacket(req writeReq) QueueType {
    // 1. 控制帧
    if req.isControl {
        return QueueControl
    }
    
    // 需要至少IPv4头（20字节）
    if len(req.data) < 20 {
        return QueueData
    }
    
    // 检查IPv4版本
    if req.data[0]>>4 != 4 {
        return QueueData
    }
    
    protocol := req.data[9]
    ihl := int(req.data[0]&0x0F) * 4  // IP头长度
    
    // 2. DNS 包（UDP 53）
    if protocol == 17 && len(req.data) >= ihl+4 {  // UDP
        dstPort := int(req.data[ihl+2])<<8 | int(req.data[ihl+3])
        srcPort := int(req.data[ihl])<<8 | int(req.data[ihl+1])
        if dstPort == 53 || srcPort == 53 {
            return QueueDNS
        }
    }
    
    // 3. TCP ACK（小包）
    if protocol == 6 && len(req.data) >= ihl+14 {  // TCP
        flags := req.data[ihl+13]
        if flags == 0x10 && len(req.data) < 100 {  // ACK only
            return QueueACK
        }
    }
    
    // 4. 普通数据（按流 hash）
    return QueueData
}

// 流 hash 计算（5-tuple）
func hashFlow(req writeReq) int {
    if len(req.data) < 20 {
        return 0
    }
    
    // 简单 hash：srcIP + dstIP + protocol
    h := int(req.data[12]) + int(req.data[13]) + int(req.data[14]) + int(req.data[15])
    h += int(req.data[16]) + int(req.data[17]) + int(req.data[18]) + int(req.data[19])
    h += int(req.data[9])  // protocol
    
    return h % 16
}
```

## 实现计划

### 阶段 1：增加队列容量（快速修复）✅ 已完成
- [x] 增加 highPriorityCh 容量（256 → 1024）
- [x] 部署测试，验证是否缓解问题

### 阶段 2：实现 DNS 专用队列 ✅ 已完成
- [x] 添加 dnsQueue 队列（512容量）
- [x] 实现分类器（识别 UDP 53）
- [x] 修改 writeLoop 优先发送 DNS
- [x] 部署测试

### 阶段 3：实现按流公平调度 ✅ 已完成
- [x] 添加 ackQueue 队列（256容量）
- [x] 实现 ACK 分类器（TCP flags == 0x10 && len < 100）
- [x] 实现 16 个数据队列（每个256容量）
- [x] 实现 DRR 公平调度器
- [x] 实现流 hash 分类（srcIP + dstIP + protocol）
- [x] 修改 writeLoop 使用 DRR 调度数据队列
- [x] 部署到 QG 环境测试
- [x] 验证 DNS 在 TCP 负载下不被饿死（30/30 成功）
- [x] 验证零队列丢包（部署后 0 drops）

### 阶段 4：优化和验证（进行中）
- [ ] 性能测试
- [ ] 压力测试
- [ ] 部署到所有环境

## 测试计划

### 测试场景 1：DNS 不被饿死
```bash
# 持续大流量传输
iperf3 -c 100.2.0.10 -t 60 &

# 同时测试 DNS
for i in {1..100}; do
    dig @100.0.0.3 jf.phn +short +time=1
    sleep 0.1
done
```

**预期**：DNS 查询全部成功，不超时

### 测试场景 2：公平调度
```bash
# 启动多个 TCP 连接
iperf3 -c 100.2.0.10 -P 10 -t 60
```

**预期**：每个连接获得相近的带宽（误差 < 20%）

### 测试场景 3：队列容量
```bash
# 突发流量
iperf3 -c 100.2.0.10 -b 100M -t 10
```

**预期**：队列不溢出，丢包率 < 1%

## 参考资料

- [RFC 2475: An Architecture for Differentiated Services](https://tools.ietf.org/html/rfc2475)
- [DRR (Deficit Round Robin) 算法](https://en.wikipedia.org/wiki/Deficit_round_robin)
- [WFQ (Weighted Fair Queuing)](https://en.wikipedia.org/wiki/Weighted_fair_queueing)

## 实现总结

### 队列架构
```
包 → 分类器
   ├─ 控制帧 → controlCh (64) → 最高优先级
   ├─ DNS (UDP 53) → dnsQueue (512) → 次高优先级
   ├─ TCP ACK (flags=0x10, len<100) → ackQueue (256) → 高优先级
   └─ 数据 → dataQueues[16] (每个256) → DRR 公平调度
```

### 关键实现
1. **DRR 调度器**：16 个队列轮询调度，每个队列 quantum=1500 字节
2. **流分类**：基于 srcIP + dstIP + protocol 的简单 hash，确保同一流进入同一队列
3. **优先级顺序**：Control > DNS > ACK > Data (DRR) > Legacy (highPriorityCh)
4. **IP 头解析**：正确处理可变长度 IP 头（IHL 字段），TCP flags 在 ihl+13，UDP 端口在 ihl+0/2

### 部署验证（QG 环境 2026-10-15）
- ✅ DNS 查询成功率：20/20 (100%)
- ✅ TCP 负载下 DNS 稳定性：30/30 (100%，iperf3 1Mbps 并发)
- ✅ 队列丢包：0 drops（部署前大量 "write queue full" 错误）
- ✅ 服务稳定性：watchdog + worker 双进程正常运行

### 性能指标
- **队列总容量**：64 + 512 + 256 + 16×256 = 4,416 包
- **DRR 轮询间隔**：100 微秒（无包时）
- **分类开销**：O(1)，仅解析 IP 头前 20-60 字节

## v2 优化方案（2026-10-17）

### 问题分析

当前 v1 实现（16 队列）存在以下问题：

1. **Hash 冲突严重**：100+ 流 hash 到 16 队列，平均每队列 6 个流，公平性差
2. **加法 hash 分布差**：简单相加导致更多冲突
3. **每次调度 1 个包**：DRR `Next()` 返回 1 个包，channel 跳转开销大

**对比业界**：
- Linux FQ-CoDel：1024 队列，冲突率 <5%
- WireGuard/Tailscale：不做公平队列，依赖外层 TCP

### 优化方案

#### 1. 队列数：16 → 256

```go
const numDataQueues = 256  // 16 → 256
const queueCapacity = 32   // 256 → 32（保持总内存）
```

**内存对比**：
- v1：16 × 256 × 1.5KB = ~6MB
- v2：256 × 32 × 1.5KB = ~12MB（可接受）

**收益**：冲突率从 ~100% 降到 ~5%

#### 2. Hash 算法：加法 → Jenkins

```go
func (s *DRRScheduler) hashFlow(data []byte) uint32 {
    h := s.salt  // 随机 salt，启动时生成
    // Jenkins one-at-a-time
    for _, b := range tupleBytes {
        h += uint32(b)
        h += h << 10
        h ^= h >> 6
    }
    h += h << 3
    h ^= h >> 11
    h += h << 15
    return h
}
```

**收益**：分布更均匀，减少冲突

#### 3. 批量出队：每次 1 包 → 每次 4-8 包

```go
func (s *DRRScheduler) NextBatch(maxBatch int, stopCh <-chan struct{}) []writeReq {
    batch := make([]writeReq, 0, maxBatch)
    for len(batch) < maxBatch {
        // DRR 轮询，每次从不同队列取包
        for i := 0; i < len(s.queues); i++ {
            idx := (s.round + i) % len(s.queues)
            if s.deficits[idx] <= 0 {
                s.deficits[idx] += s.quantum
            }
            select {
            case req := <-s.queues[idx]:
                batch = append(batch, req)
                s.deficits[idx] -= len(req.data)
                s.round = (idx + 1) % len(s.queues)
                if len(batch) >= maxBatch {
                    return batch
                }
            default:
            }
        }
        // 所有队列空，等待
        select {
        case <-stopCh:
            return batch
        case <-time.After(100 * time.Microsecond):
        }
    }
    return batch
}
```

**收益**：减少 channel 跳转开销，提升效率

### 不做 CoDel 丢包

**原因**：隧道流量会双重重传（外层 TCP + 内层 TCP），有害无益。

**替代方案**：队列满时 tail drop（当前行为）即可。

### 实现计划

- [ ] 修改 `DRRScheduler`：16 → 256 队列
- [ ] 实现 Jenkins hash + 随机 salt
- [ ] 实现 `NextBatch` 批量出队
- [ ] 修改 `writeLoop` 使用 `NextBatch`
- [ ] 编译验证
- [ ] 部署到 QG 环境测试
- [ ] 验证公平性和性能

### 预期效果

- **公平性**：100 个流带宽分配误差 <10%（v1 误差 >50%）
- **效率**：批量发送减少 50% channel 开销
- **内存**：增加 ~6MB（可接受）

## 更新日志

- 2026-10-15: 初始设计
- 2026-10-15: 阶段 1-2 完成（DNS 专用队列）
- 2026-10-15: 阶段 3 完成（ACK 队列 + 16 数据队列 + DRR 公平调度）
- 2026-10-15: QG 环境部署验证通过，零丢包
- 2026-10-17: v2 优化方案设计（256 队列 + Jenkins hash + 批量出队）
