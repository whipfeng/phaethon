# 数据面正确性批次（Batch 2）设计

> 版本: 0.1.0
> 日期: 2026-11-09
> 状态: DRAFT
> 负责人: Phaethon Dev

## 变更记录

| 版本 | 日期 | 变更内容 | 作者 |
|------|------|----------|------|
| 0.1.0 | 2026-11-09 | 初始版本：由审计 D6/D7/D9/D10/D21 收敛 | Dev |

## 1. 目标

- **D7**：matcher 配置热路径改为不可变快照 + 原子替换，消除数据面并发读写 map 的 fatal 风险。
- **D9**：P2P 删除/清理串行化到专用后台协程，绝不在全局锁内调用 mesh/netstack。
- **D10**：多链路发送按流整组路由，避免同一条流的帧跨路径乱序。
- **D21**：入站数据帧满队列即丢（不在读循环等待）、控制帧优先、入站加字节预算。
- **D6**：出站包引用泄漏修复；入站真零拷贝（改 gvisor-fork 增加外部内存 chunk + release 回调）。

## 2. 关键约束（实施前必须核实）

- `PeerSender.Send` 是异步入队（`writeReq.data` 进入 DRR 队列后才发送），
  出站不能“Send 返回即 Release”。需要确认发送完成后的释放点，或把释放绑定到发送完成。
- gVisor 契约：`LinkEndpoint.WritePackets` 拥有传入的 `PacketBufferList` 并负责 `DecRef`
  （`gvisor-fork/pkg/tcpip/stack/registration.go:1169-1171`）。
- 入站零拷贝需要 fork 侧支持“外部内存 chunk”：引用计数归零时回调归还 P2P 池，
  COW/Clone 仍复制到池化 chunk。

## 3. 设计要点

### 3.1 matcher 快照（D7）

- 新增不可变 `matcherSnapshot`（proxies、groups、matchers、resolvers 的只读副本）。
- 写路径在 `m.mu` 内修改后构建新快照并 `atomic.Store`；读路径只读快照，无锁。
- 健康检查组对象被替换时保留运行态（配合 D14，后续批次落地）。

### 3.2 P2P 清理串行化（D9）

- 删除 peer：锁内只摘表 + 投递清理请求；清理协程串行执行 mesh 下线、删 NIC、停会话。

### 3.3 按流整组路由（D10）

- 批次内按内层流分组；每条流统一选择 lane 或 primary，不做“部分帧回退”。
- lane 不可用/已满时整条流改走 primary。

### 3.4 入站队列（D21）

- 数据帧入队失败立即丢弃（mesh 数据由内层 TCP 重传兜底）。
- 控制帧优先、不丢。
- 入站按字节预算（与出站一致的 8MB/会话）。
- 顺带落地 D11 的资源上限：最大入站会话数（默认 64，可配置）、每会话 lane 上限 8、
  数据队列延迟到首个数据帧创建。

### 3.5 包内存（D6）

- 出站：`link_nic.go` / `netstack.go` / `htunnel_endpoint.go` 补 `ToBuffer()` 的 `Release()`；
  `link_nic` 的 `WritePackets` 补 `pkt.DecRef()`。
- 入站：fork 新增 `MakeWithExternalData(data, release)`；P2P 入站把池块交给它，
  引用计数归零时归还池；`HandleMeshFrame` 增加 owned 版本，丢弃路径负责归还。

## 4. 验收

- `go test -race ./db ./p2p ./mesh` 无数据竞争报告。
- 大流量压测：进程内存不随包数单调增长；无跨路径乱序导致的内层重传风暴。
- 配置热更新期间持续压测不崩溃。
- 三平台编译通过。
