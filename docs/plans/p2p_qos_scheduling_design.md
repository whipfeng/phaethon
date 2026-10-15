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
    quantum     int  // 每个队列每次发送的字节数
}

func (s *DRRScheduler) Next() *writeReq {
    for i := 0; i < 16; i++ {
        if s.deficits[i] <= 0 {
            s.deficits[i] += s.quantum
        }
        
        select {
        case req := <-s.queues[i]:
            s.deficits[i] -= len(req.data)
            return &req
        default:
        }
    }
    return nil
}
```

### 分类器

```go
func classifyPacket(req writeReq) QueueType {
    // 1. 控制帧
    if req.isControl {
        return QueueControl
    }
    
    // 2. DNS 包（UDP 53）
    if len(req.data) >= 20 {
        protocol := req.data[9]
        if protocol == 17 {  // UDP
            dstPort := binary.BigEndian.Uint16(req.data[18:20])
            if dstPort == 53 {
                return QueueDNS
            }
        }
    }
    
    // 3. TCP ACK（小包）
    if len(req.data) >= 20 {
        protocol := req.data[9]
        if protocol == 6 {  // TCP
            flags := req.data[33]
            if flags == 0x10 && len(req.data) < 100 {  // ACK
                return QueueACK
            }
        }
    }
    
    // 4. 普通数据（按流 hash）
    return QueueData
}
```

## 实现计划

### 阶段 1：增加队列容量（快速修复）
- [ ] 增加 highPriorityCh 容量（256 → 1024）
- [ ] 部署测试，验证是否缓解问题

### 阶段 2：实现 DNS 专用队列
- [ ] 添加 dnsQueue 队列
- [ ] 实现分类器（识别 UDP 53）
- [ ] 修改 writeLoop 优先发送 DNS
- [ ] 部署测试

### 阶段 3：实现按流公平调度
- [ ] 实现 DRR 调度器
- [ ] 实现 16 个数据队列
- [ ] 实现流 hash 分类
- [ ] 部署测试

### 阶段 4：优化和验证
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

## 更新日志

- 2026-10-15: 初始设计
