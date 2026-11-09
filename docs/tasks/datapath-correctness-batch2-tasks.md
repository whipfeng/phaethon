# 数据面正确性批次（Batch 2）任务

> 创建日期: 2026-11-09
> 关联方案: [datapath-correctness-batch2.md](../plans/datapath-correctness-batch2.md)
> 关联审计: [full-code-audit-20261109.md](../issues/full-code-audit-20261109.md)
> 状态: 代码完成，待部署验证（2026-11-09）

## 阶段 1: matcher 快照（D7）— 完成

- [x] 定义不可变 `ruleSnapshot`（proxies/groups/subscriptions/mappings/rules/resolvers/matchers）
- [x] `RuleMatcher` 增加 `atomic.Pointer[ruleSnapshot]`
- [x] 写路径统一 `publishLocked()`（Add/Remove/onConfigChange/rewire）
- [x] 读路径 `ResolveMatch/resolveName/resolving/findMapping/GetGroup/LookupProxy/ListGroups` 改读快照
- [x] 组解析回调（init 与 onConfigChange）改为无锁 `lookupProxy/lookupGroup/lookupSubscription`
- [x] `go test ./db/...` 通过

## 阶段 2: P2P 清理串行化（D9）— 完成

- [x] 新增 `cleanupCh` + 专用清理协程
- [x] 删除 peer 时锁内只摘表，锁外投递清理请求
- [x] mesh 下线 / 删 NIC 移出 `m.mu`（含 StartPeer 外层 defer）
- [x] `Stop()` 关闭并等待清理协程
- [x] `go test ./p2p/...` 通过

## 阶段 3: 按流整组路由（D10）— 完成（最小实现）

- [x] lane 入队非阻塞；满/失效时整组回退主链路并固定该流
- [x] 删除 50ms 写路径等待
- [x] 更新对应单测语义
- [ ] 抓包验证同流跨批次乱序已消除（待部署验证）

## 阶段 4: 入站队列与资源上限（D21/D11）— 完成

- [x] 数据帧非阻塞入队，满即丢
- [x] 控制/DNS 帧独立队列、drop-on-full 保持
- [x] 入站全局字节预算 64MB
- [x] 最大入站会话数（默认 64，`PHAETHON_MAX_P2P_SESSIONS` 可覆盖）
- [x] 每会话 lane 上限 8（被动侧 attach 拒绝并关闭）
- [x] 数据队列延迟到首个数据帧创建（`ensureDataQueues/resetDataQueues`，DRR 队列指针原子化）
- [x] `go test ./p2p/...` 通过

## 阶段 5: 包内存（D6）— 完成

- [x] `netstack.go` 写 TUN 路径补 `defer buf.Release()`
- [x] `link_nic.go` / `htunnel_endpoint.go` 复制载荷后释放 `ToBuffer` 引用
- [x] 更正审计结论：本 fork 包由调用方拥有，端点不得 `DecRef`
- [x] `gvisor-fork/pkg/buffer` 新增 `MakeWithExternalData`（外部内存 chunk + release 回调，COW 复制到池化 chunk）
- [x] `LinkNIC.InjectInboundOwned` + `MeshManager.HandleMeshFrameOwned`（丢弃路径负责归还，正常路径交给 gVisor 引用计数）
- [x] `meshInboundLoop` 对 `ownedMeshHandler` 走零拷贝，旧 handler 回退拷贝路径
- [x] `go test ./p2p/... ./mesh/...` 通过

## 阶段 6: 数据面小 bug — 完成

- [x] `mesh/link_nic.go` 发送失败计数（`sendFailures`）
- [x] `p2p/stripe_transport.go` 关闭时归还池化缓冲（baseReadLoop/laneReadLoop）
- [x] `mesh/netstack.go` 非 IPv4 / 无设备丢弃分类计数

## 阶段 7: 验证 — 完成

- [x] `go test ./db/... ./p2p/...` 通过
- [x] `go test -vet=off ./mesh/...` 通过（mesh 包存在既有 vet 报错，非本批引入）
- [x] Windows / Linux / Darwin 三平台编译通过
- [ ] 压测：内存不增长、无乱序风暴（待部署验证）
