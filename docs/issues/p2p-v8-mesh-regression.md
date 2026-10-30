# P2P v8 重写与 Mesh 热重载审查发现（10 项）

**创建时间**: 2026-10-30
**状态**: 诊断中
**优先级**: 高

## 问题描述

2026-10-30 对提交范围 `17821e1..ffbf566`（共 5 个提交：mesh config 热重载、node-id/subnet 提示重启、subnet 冲突自动重分配、P2P 协议 v8 重写、P2P 开关热生效 + admin 错误显示）进行高召回代码审查（3 个独立角度并行扫描：逐行 diff、被删行为审计、跨文件调用追踪；6 个独立验证 agent 复核 + 本地 grep/go vet 实证），共发现 **10 项问题，其中阻断级 1 项**。

审查范围：`git diff 17821e1..HEAD`，10 个文件，+972/-964。设计基准：`docs/plans/p2p_v8_simplified_protocol.md`。

## 核心流程（正确理解）

**v8 协议设计意图**（设计文档）：
- 删除 hello 握手帧与帧级 seq/ack 可靠传输（reliable.go 501 行属有意删除，由 ping/pong 覆盖保活与质量测量）
- 设计 §5.3 要求：**收到第一个 gossip 时在 gossip 处理器内完成 peer 注册**（创建 peerSender → `SetMeshSender` → `meshHandler.RegisterPeer` → `CreateLinkNIC`）
- ping/pong 用于链路质量统计（SRTT/丢包率），供 mesh 选路消费

**mesh 域名访问链路**（agents.md 已验证机制）：
DNSHijacker → fake-IP → 发起方 RouteSelector → P2P 链路 → 对端 HandleMeshFrame → InjectMeshPacket → tcp.Forwarder → handleConn 恢复域名 → adminHandler.ServeConn

**两个已推翻的设计决策**：
- 26b142d 设计为"node-id/subnet 修改后提示重启，不立即生效"；ffbf566 改为热生效，但本地网络栈重配未补齐
- 26b142d 预留的 `restart_required` 前端提示成为死代码

## 待确认问题清单

### 问题1（阻断）：v8 删除 handleHello 后未按设计 §5.3 重建 peer 注册，mesh 数据面整体失效

**假设**：v7 的 handleHello 承担 peer 注册（SetMeshSender/RegisterPeer/CreateLinkNIC/版本校验/Seq 协商），v8 删除后这些调用无任何替代路径，`peer.GetMeshSender()` 恒为 nil。

**需要验证**：
- [x] 全代码库 grep：`SetMeshSender` 唯一调用点是 `p2p/p2p.go:1356` 清理路径置 nil，从未设非 nil 值；`RegisterPeer`/`CreateLinkNIC` 仅有 mesh 侧定义，p2p 侧零调用（2026-10-30 本地实证）
- [ ] 运行时日志实锤：两节点互联后 grep gossip 处理日志，确认 sender==nil 静默丢弃（违反 §3.7，静态证据已充分，运行时证据待补）
- [ ] 修复后 mesh 端到端验证（域名互访 API）

**诊断方法**：
1. 静态：grep 已实证（见上）
2. 运行时：QG/VM 各起 v8 节点，观察 `handleGossip` 路径日志（如有）、mesh 拓扑是否建立、`GetPeerStatus` 的 nodeId 是否为空
3. 后果链：`handleGossip`（p2p.go:1552 周边）sender 守卫恒假丢弃 gossip；`BroadcastGossipToAll`/`ListMeshPeerIDs` 的 meshSenderPtr 过滤恒假；`FrameMeshPacket` 路径 `GetLinkNIC()` 恒 nil；peer 状态卡 connecting

**当前状态**: ✅ 已验证（静态代码实证，三方独立审查交叉确认；运行时日志实锤待补）

### 问题2：server 包测试编译断裂

**假设**：`frame.FrameHello`/`FrameAck` 在 frame.go 精简中删除，但 `server/htunnel_mesh_test.go` 仍引用。

**需要验证**：
- [x] `go vet ./server/` 实测报错：`undefined: frame.FrameHello`（2026-10-30 本地实证，144/163 行）

**诊断方法**：修复方式二选一：更新测试到 v8 帧格式，或删除过时的握手测试段。

**当前状态**: ✅ 已验证

### 问题3（严重）：subnet/node-id 热生效不闭环——只改状态并广播，不重建本地网络栈

**假设**：`identityUpdate` 分支（mesh.go:2237-2301）与 `reselectSubnet`（mesh.go:499-540，27d817f 引入）修改 m.subnet/m.vip 并 BroadcastGossipToAll 后，本地 gVisor NIC 地址、iptables SNAT、RouteSelector 的 LocalVIP/LocalSubnet、FakeIP pool、DNSHijacker 地址全部保持旧值。

**需要验证**：
- [x] 静态验证：identityUpdate 仅做 recomputeRoutes + SetMeshInfo + SaveState；NIC 地址只在 netstack.go:698/833 初始化绑定；SNAT 只在 720-801 初始化；RouteSelector 配置只在 SetMeshManager 时一次性捕获（netstack.go:167-180）；FakeIP/DNSHijacker 只在 NewMeshManager 用初始 subnet 创建（mesh.go:340,347）；`UpdateIdentity` 全仓库零调用（死代码）
- [x] `mesh.html:283` 检查 `data.restart_required` 但 admin.go:1822 仅返回 `{"status":"ok"}`——死代码，26b142d 残留
- [ ] 运行时验证：admin 改 subnet 后从对端 traceroute/curl 新 VIP，确认黑洞
- [ ] 设计文档 `:154` 明确把 RouteSelector 配置列为 subnet 变更需重建项——需向用户确认修复方向（补齐重建 vs 恢复"提示重启"）

**诊断方法**：
1. 静态已证（见上）
2. 运行时：改 subnet 后对端 `curl https://<node>.phn`，本机抓日志看 isLocalNetstackAddr 失配
3. 决策点：**这是方案级问题**——按铁律 §3.8 需用户决策：A) 补齐本地重配（工作量大）B) 恢复 26b142d 的"提示重启"语义（改回 + 激活 restart_required 提示）

**当前状态**: ✅ 已验证（静态）；修复方案待用户决策

### 问题4（严重）：identityUpdate 数据竞争——gossipWorker 无锁写 m.nodeID

**假设**：gossipWorker（独立 goroutine，mesh.go:2216）中 identityUpdate 的 nodeID 分支无锁写 `m.nodeID`（:2246）、锁外读 `m.subnetStr`（:2241,2270,2277,2281,2286）；subnet 分支锁内写 subnet/vip。读取方 GetNodeID(:571)/GetSubnet(:584)/GetVIP(:568)/GetStatus(:1253)/isLocalVIP(:794)/HandleTopologyGossip(:1233) 及 main_engine.go:148 全部无锁。

**需要验证**：
- [x] 静态验证：上述行号锁约定核实无误；`m.vip` 是 `net.IP` 切片，无锁并发写可撕裂读；热重载前 m.nodeID 仅 init 期持锁写一次，热重载引入了新的并发写路径
- [ ] 运行时：`go build -race` 部署到测试环境，admin 改 node-id 触发 identityUpdate，观察 race detector 报告

**诊断方法**：修复方向（待授权）：统一锁约定——identityUpdate 全程持 m.mu；或所有读取方改 RLock；m.vip 等切片字段避免无锁读写。

**当前状态**: ✅ 已验证（静态）

### 问题5（严重）：Seq/RemoteSeq/LinkID 协商随 hello 一起被删，mesh 仍依赖

**假设**：v7 hello 帧携带 Seq 并推导 RemoteSeq/LinkID（`git show 2a0589e^:p2p/p2p.go:1765-1777`），v8 删除后无任何赋值路径（测试桩除外）；GossipInfo.Seq 无人填充（broadcastGossipToPeer :1848-1851 不填，mesh BuildGossipInfo :2902-2952 也不填）。mesh.go 仍用 GetLocalSeq/GetRemoteSeq 构建 GossipLink（:1401,1619,2395,2644,2685,2934,3114,3217）并做 seq 对反向链路匹配（:2472,3174）。

**需要验证**：
- [x] 静态验证：全仓 grep `RemoteSeq\s*=`/`LinkID` 赋值零命中（生产代码）；设计文档全文无 localSeq/remoteSeq 废弃声明（属连带删除而非有意废弃）；docs/plans/libp2p-integration-design.md:1268 明确"保留 GetLocalSeq/GetRemoteSeq 用于向后兼容"
- [ ] 运行时：两节点互通告拓扑后，检查 GossipLink 的 remoteSeq 是否恒 0、反向链路确认是否失效
- [ ] 决策点：修复方向待用户决策（gossip 帧恢复 Seq 字段 vs mesh 侧改为新协商机制）

**诊断方法**：grep + 对照设计文档 + 运行时 gossip 日志（含 GossipLink 序列化内容）

**当前状态**: ✅ 已验证（静态）

### 问题6：LastError 成功时不清除 + 读写数据竞争

**假设**：v7 handleHello 成功路径 `peer.LastError = ""`（`git show 17821e1:p2p/p2p.go:1753`）随 handleHello 删除而丢失；当前仅 4 处赋值全部为错误字符串（:557 写失败、:1290 拨号失败、:1449 心跳超时、:1490 Recv 错误）。ffbf566 新增 admin 错误显示（GetPeers 返回 lastError，:1898），恢复后的连接**永久显示过期错误**。同时 GetPeers 持 m.mu 复制读 LastError，写方（:557/:1449/:1490）不在 m.mu 临界区内——锁不交叠，真实数据竞争。

**需要验证**：
- [x] 静态验证：v7 清除点存在、v8 无任何成功路径清空；读写锁不交叠
- [ ] 运行时：admin 面板观察一次"拨号失败→恢复"的连接，确认错误信息滞留
- [ ] 修复后回归

**诊断方法**：修复方向（待授权）：P2P 拨号/会话循环成功路径补 `peer.LastError = ""`（建议加 Peer 方法内聚管理 + 原子/锁保护）

**当前状态**: ✅ 已验证（静态）

### 问题7：ping/pong 丢包率统计虚高，误导 Dijkstra 选路

**假设**：recordPing（:1651-1657）5 秒清理 pendingPings 但不回滚 totalSent；recordPong 仅在 map 命中时 totalReceived++；sendPing 先 recordPing 后 enqueueWrite（返回值忽略，失败也计入 sent）；pingTracker 无窗口化/重置（newPingTracker 仅建 Peer 时调一次）。高 RTT（>5s）链路 lossRate 趋近 1 且永不回落。lossRate 经 GetLinkQualityStatsByProxy → mesh quality.UpdateACKStats → `cost = srtt/(1-lossRate)`（mesh.go:1417-1418 等）驱动 Dijkstra，cost→∞ 永久毒化选路。

**需要验证**：
- [x] 静态验证：上述机制核实无误
- [ ] 运行时：构造高 RTT 链路（经拥塞代理），观察 GetLinkQualityStats 的 loss 值与实际不符
- [ ] 决策点：修复方向待用户决策（滑动窗口 vs 老化衰减 vs 清理时回滚 totalSent）

**诊断方法**：单测可直接覆盖（构造 pendingPings 老化场景断言 lossRate）；运行时对照真实链路质量

**当前状态**: ✅ 已验证（静态）

### 问题8：协议版本号 8 与旧 v9 撞车，且 v8 删除版本校验

**假设**：p2p.go:27-48 注释块最新条目仍是 "Version 9: GossipLink changed from linkId to localSeq+remoteSeq fields"，但 `P2PProtocolVersion = 8`——旧 v9 节点与新 v8 节点版本号相同但 wire 格式不同（gossip GossipLink 字段不同）；v7 的版本不匹配处理（transport.Close + Warn）在 v8 无替代。滚动升级混部署时：旧节点断开并记日志，新节点不检查、无日志，形成无声重连循环。

**需要验证**：
- [x] 静态验证：注释与常量矛盾实证；v8 gossip 路径无任何 ProtocolVersion 比对逻辑
- [ ] 确认线上是否存在 v9 节点（查各环境二进制版本：`strings ./phaethon | grep -E 'v0\.[0-9]+'`）
- [ ] 决策点：版本号定为多少（9？10？）、混部署期行为待用户决策

**诊断方法**：grep + 各环境版本核查 + 混部署测试

**当前状态**: ✅ 已验证（静态）；影响面评估待确认

### 问题9：proxyConfigChanged 遗漏 trojan Servername

**假设**：main.go:1941-1960 逐字段比较仅含 Type/Server/Port/Password/Sni/URL/ViaProxy/SkipCertVerify/Username/Flow/IsP2P/IsV2。遗漏 `Servername`（trojan 在 sni 为空时用作 SNI 回退，dialer/trojan.go:133-136）。

**需要验证**：
- [x] 静态验证：比较字段清单核实；遗漏字段中 UUID/PrivateKey/Fingerprint/RealityOpts/Cipher/Tfo 仅 VLESS/SSH/SS 使用，而这些类型不启动 P2P peer（IsP2P 仅 socks5/trojan/h_tunnel）——实际影响面仅 trojan Servername
- [ ] 运行时：admin 改 trojan servername 后观察 peer 是否用旧 SNI 拨号

**诊断方法**：修复方向（待授权）：proxyConfigChanged 增加 Servername 比较（一处改动）

**当前状态**: ✅ 已验证（静态）

### 问题10：ChangeIdentity 事件 channel 满时静默丢弃，库面不一致无暴露

**假设**：ChangeIdentity（mesh.go:611-623）select/default 投递 normalCh（容量 1024），满则仅 warn；main.go:2225 忽略返回值直接 return nil；admin.go:1793 先写库、:1822 返回 `{"status":"ok"}`，成功路径不依赖 ChangeIdentity 结果。channel 满时：数据库新配置、运行态旧身份、界面显示成功、无重试。

**需要验证**：
- [x] 静态验证：代码路径属实；normalCh 消费为阻塞即时接收，单事件耗时随 peer 数增长——触发概率低（PLAUSIBLE 降级），但无任何暴露/重试机制
- [ ] 压力场景验证：大规模 mesh + 频繁事件时 admin 改 node-id

**诊断方法**：修复方向（待授权）：ChangeIdentity 失败时 admin 返回 503/同步等待 ack/重试

**当前状态**: PLAUSIBLE（静态事实确认，触发条件现实罕见）

### 补充项（未单独验证）：设计 §4.4 承诺的"连续多 ping 无响应判定链路断开"未实现

pingTracker 统计了丢包率但无消费方据此断链；故障检测只靠 30 秒全静默 heartbeat。半死链路（gossip 可达但数据面全丢）不会断开。来源：审查 finder 单方发现，未经独立验证，待与用户确认是有意简化还是遗漏。

**当前状态**: ❌ 未验证

## 诊断日志位置

- **QG**: `/root/data/logs/phaethon.log`（`ssh -p 2222 root@10.11.61.50`）
- **VM**: `C:\Users\Docker\Desktop\Workspace\phaethon\data\logs\phaethon.log`（`ssh -p 39022 Docker@10.21.20.65`）
- **GG**: `/home/layer4/phaethon-gg/data/logs/phaethon.log`
- **JF**: `/home/layer4/layer4-deploy/data/logs/phaethon.log`
- 根目录 `phaethon.log` 仅为 stdout/stderr，不能替代主日志
- 验证 P2P 相关问题时 grep 关键字：`handleGossip`、`SetMeshSender`、`GossipLink`、`identityUpdate`、`recordPing`/`recordPong`

## 诊断步骤计划

**Phase 0（已完成，2026-10-30）**：静态审查 + 本地实证（grep / go vet），产出本文档。

**Phase 1（待授权）**：运行时日志实锤
1. 问题1：VM 环境部署当前 master，两节点互联，确认 mesh 拓扑无法建立（gossip 丢弃、无 LinkNIC 创建日志）
2. 问题4：`go build -race` 到 VM，admin 改 node-id 触发 race 报告
3. 问题6/7：admin 面板观察 LastError 滞留 + 构造高 RTT 场景核对 loss 统计

**Phase 2（待用户决策）**：方案级问题（问题3、问题5、问题8）修复方向决策——按铁律 §3.8 请示后先改 `docs/plans/p2p_v8_simplified_protocol.md` 再改代码。

**Phase 3（待授权）**：按优先级修复（建议顺序：1 → 2 → 3/4/5 → 6/7 → 8/9/10），每步修复后重新编译部署验证。

**部署验证门控（§3.6）**：在问题1修复并验证前，**master 的 v8 代码不具备部署到任何环境的条件**。

## 结论与修复

### 方案决策（2026-10-30，用户本人决策）

| 问题 | 决策 | 说明 |
|------|------|------|
| 问题1 注册丢失 | 按设计文档 §5.3 在 handleGossip 首个 gossip 分支重建注册 | 设计本就有，实现遗漏 |
| 问题2 测试编译断裂 | 修复：frame 常量引用更新为 v8 帧格式 | — |
| 问题3 热生效不闭环 | **B：恢复"提示重启"语义**（subnet 禁止热生效，admin 返回 restart_required；NodeID 维持热更新） | 设计文档 §3.3 已加实现红线 |
| 问题4 数据竞争 | identityUpdate/reselectSubnet 全程持 m.mu；GetNodeID/GetSubnet/GetVIP/GetStatus 等读取方改 RLock | 标准并发修复 |
| 问题5 Seq 协商丢失 | gossip 帧携带本端 Seq；首个 gossip 协商 RemoteSeq/LinkID | 不改协议格式（Seq 字段已存在） |
| 问题6 LastError | 成功路径补清除 + 加锁保护 | — |
| 问题7 loss 统计 | **一步到位**：清理回滚 + 滑动窗口（最近 20 次） | 设计文档 §4.3 已修订 |
| 问题8 版本号 | **定为 10**（避让线上 v9），恢复版本不匹配断开+Warn 日志 | 设计文档 §2.3/§8.1 已修订 |
| 问题9 proxyConfigChanged | 比较清单补 trojan Servername | 一行改动 |
| 问题10 ChangeIdentity | 事件丢弃时 admin API 返回 503，不假成功 | — |
| 补充项 ping 断链 | **本次实现**（2026-10-30 用户修订，原决策"后续增强"）：连续 10 ping 无 pong → Warn + 关连接走现有断开路径；30s 心跳保留兜底 | 设计文档 §4.4 已修订 |
| 问题3 附带 | nodeID 热更需补齐身份烧入点刷新：`ConnectionHandler.localMeshNodeID` 需新增 setter（当前启动缓存无刷新机制，netstack.go:528 setter 为死代码）；domainTrie 重建需清除旧 nodeID 静态条目；mTLS CN=旧 nodeID 证书失效（all 证书不受影响） | 2026-10-30 代码查证 |

### 修复执行顺序（待用户最终确认设计文档后开工）

1. 问题1（阻断：gossip 注册重建）→ 2. 问题2（测试编译）→ 3. 问题3B（subnet 恢复重启语义 + restart_required；nodeID 补 handler/domainTrie 刷新）→ 4. 问题4（锁）→ 5. 问题5（Seq 协商）→ 6. 问题6（LastError）→ 7. 问题7（滑动窗口 loss）→ 8. 问题8（版本10+校验）→ 9. 问题9+10 → 10. 补充项（多 ping 断链）

修复完成后统一编译 + VM 部署验证（用户决策 10：修完再验），验证项：mesh 端到端拓扑建立、race 构建下 admin 改 node-id、LastError 恢复清除、高 RTT 场景 loss 回落。

（Phase 1-3 执行结果待修复推进后填写）

## 更新日志

- **2026-10-30**：文档创建。Phase 0 完成：3 角度并行审查（逐行扫描/被删行为审计/跨文件追踪）+ 6 验证 agent 复核 + 本地实证，确认 10 项发现（9 项静态已验证，1 项 PLAUSIBLE，1 项补充未验证）。其中问题1/2 有本地实证（grep / go vet）。
- **2026-10-30（同日）**：用户完成全部方案决策（见"方案决策"表）。设计文档 `docs/plans/p2p_v8_simplified_protocol.md` 已按决策修订：版本 8→10、GossipInfo 补 Seq、§3.3 subnet 禁止热生效红线、§4.3 滑动窗口、§4.4 故障检测标注后续增强。待用户确认设计文档后进入代码修复阶段（修完统一 VM 验证）。
- **2026-10-30（二次修订）**：用户质疑两点，查证后修订决策：①多 ping 断链**由后续增强改为本次实现**（连续 10 ping 无 pong 断链，§4.4 已细化）；②nodeID 热更保留，但代码查证确认 `ConnectionHandler.localMeshNodeID` 启动缓存无刷新机制（netstack setter 为死代码）、domainTrie 需清除旧条目——已列入问题3 修复范围。subnet 重启决策维持不变（转发平面 vs 身份缓存的本质区别）。
