# P2P QoS 调度实现任务

**创建时间**: 2026-10-15  
**关联设计**: `docs/plans/p2p_qos_scheduling_design.md`

## 任务清单

### 阶段 1：快速修复（增加队列容量）
- [ ] Task 1.1: 增加 highPriorityCh 容量（256 → 1024）
- [ ] Task 1.2: 编译验证
- [ ] Task 1.3: 部署到 QG 环境
- [ ] Task 1.4: 测试 DNS 稳定性

### 阶段 2：DNS 专用队列
- [ ] Task 2.1: 添加 dnsQueue 队列定义
- [ ] Task 2.2: 实现包分类器（识别 UDP 53）
- [ ] Task 2.3: 修改 enqueueWriteReq 支持 DNS 分类
- [ ] Task 2.4: 修改 writeLoop 优先发送 DNS
- [ ] Task 2.5: 编译验证
- [ ] Task 2.6: 部署到 QG 环境
- [ ] Task 2.7: 测试 DNS 不被饿死

### 阶段 3：按流公平调度
- [ ] Task 3.1: 实现 DRR 调度器
- [ ] Task 3.2: 实现 16 个数据队列
- [ ] Task 3.3: 实现流 hash 分类
- [ ] Task 3.4: 集成到 writeLoop
- [ ] Task 3.5: 编译验证
- [ ] Task 3.6: 部署到 QG 环境
- [ ] Task 3.7: 测试公平调度

### 阶段 4：全面验证
- [ ] Task 4.1: 性能测试
- [ ] Task 4.2: 压力测试
- [ ] Task 4.3: 部署到 VM 环境
- [ ] Task 4.4: 部署到 GG 环境
- [ ] Task 4.5: 部署到 JF 环境
- [ ] Task 4.6: 全面验证

## 实现细节

### Task 1.1: 增加 highPriorityCh 容量

**文件**: `p2p/p2p.go`

**修改点**:
```go
// Line 642
highPriorityCh: make(chan writeReq, 1024),  // 256 → 1024

// Line 867
peer.highPriorityCh = make(chan writeReq, 1024)  // 256 → 1024
```

**验证**:
```bash
make linux
# 部署到 QG
ssh -p 2222 root@192.168.1.101 "pkill -9 phaethon; sleep 2; cd /root && ./phaethon &"
```

### Task 2.1-2.4: DNS 专用队列

**文件**: `p2p/p2p.go`

**修改点**:
1. 添加 dnsQueue 定义
2. 实现 classifyPacket 函数
3. 修改 enqueueWriteReq 支持 DNS 分类
4. 修改 writeLoop 优先发送 DNS

### Task 3.1-3.4: DRR 调度器

**文件**: `p2p/drr_scheduler.go`（新建）

**实现**:
- DRR 调度器结构
- 16 个数据队列
- 流 hash 分类
- 公平调度算法

## 测试计划

### 测试场景 1: DNS 不被饿死
```bash
# 持续大流量
iperf3 -c 100.2.0.10 -t 60 &

# 同时测试 DNS
for i in {1..100}; do
    dig @100.0.0.3 jf.phn +short +time=1
    sleep 0.1
done
```

### 测试场景 2: 公平调度
```bash
# 多个 TCP 连接
iperf3 -c 100.2.0.10 -P 10 -t 60
```

## 进度跟踪

- 2026-10-15: 创建设计文档和任务文档
