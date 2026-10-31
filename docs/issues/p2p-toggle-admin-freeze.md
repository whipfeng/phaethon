# P2P 开关保存卡死：根因分析与监听重构方案

**创建时间**: 2026-11-01
**状态**: 已实施，待部署验证
**优先级**: 高（域名访问控制台必现）

## 问题描述

域名访问控制台，修改代理的 P2P 开关点保存：编辑页面不消失，且控制台切换其它页面也卡住。

## 根因（两因素叠加，均在 `apiToggleP2P`，admin/admin.go:2301-2340）

1. **`s.mu` 锁内同步执行全量 peer 重启**：handler 持 `s.mu` 调 `OnIncrementalUpdate`（main.go:2078），后者对 DB 里所有兼容类型（socks5/trojan/h_tunnel）的 running peer **无条件** `RestartPeer`。期间所有其它 admin API 请求抢不到锁 → 全页面卡。
2. **响应写在断链之后**：admin 会话本身骑在 P2P/mesh 链路上；`RestartPeer` 是同步 `stopPeer`（当场 close transport，p2p.go:1199-1203）+ 异步重拨。`jsonResponse` 在重启之后执行 → 承载连接的 transport 已死 → 响应丢失 → 编辑页不消失。

同类写法：`apiToggleProxy`（admin.go:2293）——改 enabled 也会触发，只是用户未在域名访问下改它。

## 调用点审计（2026-11-01 grep 实证）

`OnIncrementalUpdate` 在 admin.go 共 **16 个调用点**，仅 4 个与 P2P 相关：

| 相关 | apiProxies 保存/删除（2190/2236）、apiToggleProxy（2293）、apiToggleP2P（2334） |
|---|---|
| **无关（12 个）** | apiMappings、apiToggleMapping、apiResolvers、apiToggleGroup、apiGroupSubscription、apiGroupActiveMember、apiGroups、apiToggleSubscription、apiReverse×4 |

改映射/代理组/订阅/反向配置都会全量断连重拨所有 P2P peer——纯误伤。

**全局引用确认**：`OnIncrementalUpdate` 仅 admin/admin.go（声明 + 16 调用点）与 main.go:2078（唯一赋值）引用；其唯一实现只做 P2P peer 同步。**删除无其它影响**。

## 决策记录（用户确认）

1. **删除 `OnIncrementalUpdate`**，改用 db 包现成的监听机制：`db.Subscribe(BucketProxies)` 在 main 启动时注册一次，事件自带精确 key（代理名）。
2. **admin handler 不再认识 P2P**：校验 → 写库 → 回响应，完事。P2P 同步完全由监听链驱动。
3. **去抖 worker**：监听回调只把名字入 pending 集合并重置 ~200ms 定时器；定时器到点按 DB 最新状态对每个名字做一次精确同步（该启启/该停停/该重重）。连点保存、批量写入合并为一次重启，中间态天然跳过。notify 是同步回调，重启动作必须在后台 goroutine（notify.go 注释既定约束）。
4. **不做字段级对比**：事件 key 已精确到代理，无需对比内部字段（原"重启无条件"注释中的维护陷阱由机制层面消除）。
5. **不做软切换/无缝重启**（用户决策）：主动编辑代理导致该 peer 重拨是低频且用户自发起，可接受；`RestartPeer` 维持先拆后建。协议级 stream 迁移（真无缝）不做。
6. **启动初始 peer 建立**仍走 main 自己的启动逻辑，不经监听链。

## 实施计划

- [x] main.go：新增 `db.Subscribe(BucketProxies)` + 去抖 worker（pending set + timer，出队时现读 DB 状态精确同步单个 peer）
- [x] main.go：删除 `adminSrv.OnIncrementalUpdate` 赋值块（2078-2133）
- [x] admin/admin.go：删 `OnIncrementalUpdate` 字段声明与全部 16 个调用点
- [x] `go build` + db 测试通过（2026-11-01；go vet 报的 4 处 redundant nil 检查为预存问题，与本次无关）

## 验证计划

1. 域名访问控制台，切换 P2P 开关保存：编辑页正常关闭、其它页面不卡。
2. 观察日志：仅目标代理 peer 重启一次，其它 peer 不动。
3. 连点保存 3 次：目标 peer 只重启一次（去抖生效）。
4. 保存一个映射/代理组：日志中无任何 P2P 重启动作（12 个误伤点消除）。
5. 删除一个 P2P 代理：对应 peer 停止（原 DELETE 孤儿化 bug 路径仍被覆盖——put/delete 事件都带 key）。

## VM 验证记录（2026-11-01，v0.8.0-221-g639528b）

- [x] 部署 VM，版本戳确认 `P2PManager initialized (version=v0.8.0-221-g639528b)`，无新增错误
- [x] admin API `/api/proxies` HTTP 200
- [x] 网页标题输出 `[vm] Dashboard - Phaethon Manager`（node-id 前缀生效）
- [x] 启动后 P2P peer 自动建立：QG_HT → peer established via first gossip → mesh peerCount=1
- [x] PATCH enabled 关：200 返回 36ms；日志精确单条 `stopping peer for proxy QG_HT`，peerCount 1→0
- [x] PATCH enabled 开：200 返回 35ms；日志精确单条 `starting peer for proxy QG_HT`，peer 重建、peerCount 0→1
- [ ] 域名访问控制台实测保存 P2P 开关（需等部署到对应节点后由用户操作确认）
- [ ] 连点保存去抖实测
- [ ] 保存映射/代理组无 P2P 动作实测
