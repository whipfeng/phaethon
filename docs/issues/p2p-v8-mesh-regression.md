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
| 问题3 热生效不闭环 | **B 最终版：nodeID 与 subnet 统一"保存配置 + 重启生效"**，admin 返回 restart_required，运行态保持旧身份；依赖各环境看门狗/监督进程自动拉起（重启成本秒级） | 设计文档 §3.3 已按最终决策修订（2026-10-30 三次修订） |
| 问题4 数据竞争 | **随决策消解，无需修复**：identityUpdate 运行态分支整体删除（死代码兼竞争源），nodeID/subnet 不再有无锁并发写路径 | 删除即修复 |
| 问题5 Seq 协商丢失 | gossip 帧携带本端 Seq（GossipInfo.Seq）；首个 gossip 协商 RemoteSeq/LinkID | 不改协议格式（Seq 字段已存在） |
| 问题6 LastError | 成功路径补清除 + 加锁保护 | — |
| 问题7 loss 统计 | **一步到位**：清理回滚 + 滑动窗口（最近 20 次） | 设计文档 §4.3 已修订 |
| 问题8 版本号 | **定为 10**（避让线上 v9），恢复版本不匹配断开+Warn 日志 | 设计文档 §2.3/§8.1 已修订 |
| 问题9 proxyConfigChanged | **改为：admin 保存代理配置即无条件重启该节点 P2P 连接**——删除逐字段比较逻辑（2026-10-30 用户决策）。字段比较是维护陷阱，每新增协议字段都需同步比较清单，漏项即 silent bug（Servername 即如此漏掉）；配置变更为低频操作，重启代价秒级可接受 | 比补字段更根治 |
| 问题10 ChangeIdentity | **随决策消解，无需修复**：ChangeIdentity 改为同步（存库 + 返回 restart_required），不再投递 normalCh，无事件可丢 | 删除事件路径即修复 |
| 补充项 ping 断链 | **本次实现**（2026-10-30 用户修订，原决策"后续增强"）：连续 10 ping 无 pong → Warn + 关连接走现有断开路径；30s 心跳保留兜底 | 设计文档 §4.4 已修订 |
| 问题3 附带（subnet 冲突自动重分配） | 27d817f 语义调整为：检测到冲突 → 选取新 subnet 存库 → 记日志后**进程自我退出（exit 0）** → 看门狗拉起用新身份 | 不再热改运行态 |
| 问题3 附带（nodeID 热更烧入点） | 不再需要：不热更则 `ConnectionHandler.localMeshNodeID`、domainTrie 静态条目、mTLS CN 证书均无需运行态刷新 | 重启自然重建 |

### 修复执行顺序（设计文档已按最终决策修订完毕，待用户"开工"）

1. 问题1（阻断：gossip 注册重建）→ 2. 问题2（测试编译）→ 3. 问题3B（删除 identityUpdate 运行态分支 + UpdateIdentity；ChangeIdentity 改同步返回 restart_required；subnet 冲突改存库 + 自我退出）→ 4. 问题5（gossip 填充 Seq + 首个 gossip 协商 RemoteSeq/LinkID）→ 5. 问题6（LastError 成功清除 + 加锁）→ 6. 问题7（滑动窗口 loss）→ 7. 问题8（版本 10 + handleGossip 强制版本检查）→ 8. 问题9（proxyConfigChanged 补 Servername）→ 9. 补充项（连续 10 ping 断链）

问题4、问题10 随 nodeID 重启决策消解，仅需删除死代码（已并入第 3 步清理项）。

### 路由环路风险与重算触发链分析（2026-10-30 深度讨论，用户决策）

**环路分析结论**：
- **稳态环路数学上不存在**：gossip 收敛后全网持有同一有向图（各端发布自己测的方向代价），正代价下" A 经 B 到 X 更优且 B 经 A 到 X 更优"两式相加导出矛盾（代价和 < 0），不成立。全双工独立测量**合理**，无需代价单向化（原"建议2"撤回）
- **瞬态环路**仅在 gossip 视图时差（~15s）内可能，唯一兜底是 **TTL 每跳递减**——mesh 自转发 IPIP 外层包不经过 gVisor IP 转发，TTL 是否递减未经证实，列为开工检查项
- gVisor `RouteDecisionCache`：按目标 IP 维度、无 TTL、全量清空（`ClearRouteDecisionCache`）；`stickyCache` 只存最终目标节点，下一跳由 bestPaths 实时给出
- 发现隐性触发链：`compareGossipNeighbors` 用 `!=` 整体比较 GossipLink（含 SRTT/LossRate 浮点），浮点每帧必变 → 每轮 gossip 都触发重算+清缓存。"质量驱动重算"靠副作用成立而非显式设计

**新增修复项（追加进修复顺序）**：

| 项 | 内容 | 说明 |
|----|------|------|
| 环路-1 | **验证 mesh/IPIP 转发外层 TTL 每跳递减**（开工第一项，日志实证） | 瞬态环路唯一兜底；不递减则是硬伤 |
| 环路-2 | **qualityLoop 每 10s 无条件投递 `recomputeRoute` 事件**（方案A，用户已接受） | 显式质量触发，替代浮点副作用；Dijkstra 微秒级，代价可忽略。criticalWorker 已支持该事件类型（mesh.go:2207），只是无人投递 |
| 环路-3 | `compareGossipNeighbors` 比较忽略 SRTT/LossRate/Jitter | 职责分离：gossip 只响应拓扑变化（保故障切换即时性），质量变化归 qualityLoop 周期触发 |
| 环路-4 | **IPIP 分支（分支3）`Cacheable` 改 false**，与分支2一致 | 用户决策：两者都动态选下一跳（`NextHopLink`），缓存语义应一致。原注释"IPIP egress is static per prefix"只对一半——出口静态但下一跳动态，混装动态字段的缓存条目只能靠清缓存刷新（滞后一个重算周期） |

修复完成后统一编译 + VM 部署验证（用户决策 10：修完再验），验证项：mesh 端到端拓扑建立、race 构建下 admin 改 node-id、LastError 恢复清除、高 RTT 场景 loss 回落、IPIP 转发 TTL 递减日志实证。

（Phase 1-3 执行结果待修复推进后填写）

## 更新日志

- **2026-10-30**：文档创建。Phase 0 完成：3 角度并行审查（逐行扫描/被删行为审计/跨文件追踪）+ 6 验证 agent 复核 + 本地实证，确认 10 项发现（9 项静态已验证，1 项 PLAUSIBLE，1 项补充未验证）。其中问题1/2 有本地实证（grep / go vet）。
- **2026-10-30（同日）**：用户完成全部方案决策（见"方案决策"表）。设计文档 `docs/plans/p2p_v8_simplified_protocol.md` 已按决策修订：版本 8→10、GossipInfo 补 Seq、§3.3 subnet 禁止热生效红线、§4.3 滑动窗口、§4.4 故障检测标注后续增强。待用户确认设计文档后进入代码修复阶段（修完统一 VM 验证）。
- **2026-10-30（二次修订）**：用户质疑两点，查证后修订决策：①多 ping 断链**由后续增强改为本次实现**（连续 10 ping 无 pong 断链，§4.4 已细化）；②nodeID 热更保留，但代码查证确认 `ConnectionHandler.localMeshNodeID` 启动缓存无刷新机制（netstack setter 为死代码）、domainTrie 需清除旧条目——已列入问题3 修复范围。subnet 重启决策维持不变（转发平面 vs 身份缓存的本质区别）。
- **2026-10-30（三次修订，最终决策）**：用户决策 nodeID 修改**也统一走重启生效**（"重启依赖看门狗"），与 subnet 完全对齐："保存配置 + restart_required + 运行态不变 + 看门狗自动拉起"。由此**问题4（数据竞争）与问题10（事件静默丢弃）随决策消解**——identityUpdate 运行态分支与 ChangeIdentity 事件路径均改为删除项，无并发写、无事件可丢。subnet 冲突自动重分配语义同步调整为"存库 + 自我退出（exit 0）+ 看门狗拉起"。设计文档 §3.3 与本文档决策表已按最终版修订。
- **2026-10-30（四次修订）**：问题9 修复方案由"比较清单补 Servername"改为"**admin 保存代理配置即无条件重启该节点 P2P 连接，删除 proxyConfigChanged 逐字段比较**"（用户决策：低频操作无需精细比较，字段比较清单是维护陷阱）。
- **2026-10-30（五次修订）**：路由环路深度讨论。结论：稳态环路在一致有向图+正代价下数学不存在（全双工独立测量合理，原代价单向化建议撤回）；瞬态环路依赖 TTL 兜底待验证。新增 4 项修复：①验证 IPIP/mesh 转发 TTL 递减 ②qualityLoop 每 10s 无条件投递 recomputeRoute（方案A，显式替代浮点副作用）③compareGossipNeighbors 忽略质量字段（gossip 只管拓扑）④IPIP 分支 Cacheable 改 false 与分支2一致（用户决策：两者都动态选下一跳，缓存语义应一致）。
- **2026-10-30（修复完成，VM 验证）**：全部代码修复完成并部署 VM（版本 v0.8.0-191-g88a2c28-dirty）。修复清单：问题1（handleGossip 首个 gossip 重建注册 establishPeerFromGossip + NodeID 变更重注册 reregisterPeer）→ 问题2（htunnel_mesh_test 重写为 v8 gossip 帧，通过）→ 问题3B/4/10（identityUpdate 分支、UpdateIdentity、proxyConfigChanged 删除；ChangeIdentity 改同步返回 restart_required；subnet 冲突改存库 + os.Exit(0)；admin API 已实测返回 restart_required，运行态身份不变）→ 问题5（gossip 填充 Seq，首帧协商 RemoteSeq/LinkID）→ 问题6（LastError 加锁 + 成功路径清除）→ 问题7/§4.4（pingTracker 滑动窗口 20 + 发送失败回滚不计数 + 连续 10 无 pong 断链，VM 日志已实测触发）→ 问题8（版本 10 + 强制版本检查）→ 环路-1（TTL 递减静态验证）→ 环路-2/3/4（10s 定时重算 VM 日志已实测、邻居比较忽略质量字段、branch3 Cacheable=false）。**待办**：race 构建本机与 VM 均无 gcc 无法编译（问题4 运行时 race 验证改期）；mesh 端到端、LastError 运行时观察、高 RTT 回落需 QG 升级新协议后验证（部署顺序：VM → 用户确认 → QG）。
- **2026-10-30（六次修订）**：身份变更"保存即重启"完整落地（用户决策）：①admin 保存 node-id/subnet 变更时先清 DB fakeip bucket（新增 `db.ClearFakeIPBucket`，避免旧 subnet 映射作为垃圾残留），存库后返回 `restart_required`，响应发出后经 `RequestRestart` 回调 goroutine sleep 2s + `os.Exit(0)`，watchdog 拉起新身份；②前端 mesh.html 记录当前身份，node-id/subnet 被修改时弹确认框"保存后自动重启"，**取消则不保存**（i18n 中/英补齐 `mesh.identityRestartConfirm`/`mesh.restartScheduled`/`common.cancelled`）；③VM 端到端实测通过（v0.8.0-192-gd16f8a6-dirty）：PUT node-id → 200 + restart_required → 进程退出 → watchdog 拉起新身份 → GET 确认 → 回滚同样生效。race 阻塞更新：可同步代码到 Mac 构建 race 二进制（用户告知本机即 VM 环境，缺工具链时走 Mac）。
- **2026-10-30（QG 升级，互相印证）**：QG 部署 v10（v0.8.0-194-g101279b-dirty，linux/amd64）。**QG 磁盘曾 100% 满**（p2p-cache 旧版本包 3.7GB + 轮转日志 1GB + 旧二进制 84MB），清理后 29% 占用。mesh 端到端互相印证通过：QG 侧 `peer registered: vm` + LinkNIC 创建，VM 侧 `peer registered: qg` + LinkNIC 创建，双向 gossip 15s 周期正常；数据面实测：经 VM mesh DNS 解析 `mb.qg.phn → 100.0.0.33`（gossip 域名知识收敛），并经 mesh SSH 到 Mac 成功（VM→QG→Mac 全链路）。**身份变更重启严格复测**：subnet 与 node-id 分别实测，按日志 `[MESH] started` 确认新进程以新身份启动（旧进程退出前会用新 DB 应答 admin GET，轮询 API 不可靠），FakeIP 池清空实证（身份变更后的重启无 `Loaded N FakeIP mappings` 行，池从空按序重建）。mixed-version 窗口关闭。
- **2026-10-30（七次修订，QG bounce 实测 + 劣化链路验证）**：上环境前验证清扫完成，新增实测证据：
  - **QG bounce #1**（rc-service restart）：VM 侧完整链路——`read error ... connection refused` → 删 LinkNIC + `peer unregistered: qg` → **9 秒后** `peer established via first gossip ... linkID=2-5`（问题1 首个 gossip 重建 + 问题5 首帧协商实测）→ LinkNIC 重建 → MESH-ACK `samples=3`（问题7 新连接独立 pingTracker 实测，重连前 samples=2309）。
  - **QG bounce #2** + `/api/p2p` 轮询器：问题6 LastError 运行时实测——`"" → "htunnel-direct: get fail: ... connection refused" → "io: read/write on closed pipe" → 重连成功 10 秒内恢复 `""`（拨号成功即清除，与问题6 修复一致）。
  - **环路-3 实测**：gossip 周期严格 15s（12:40:43/12:40:58/12:41:13…），邻居比较忽略质量字段后 srtt/jitter 持续变化不再驱动重算，拓扑不变时无重算风暴。
  - **环路-4 实测**：QG 两次 bounce（拓扑变化）后，mesh DNS `mb.qg.phn → 100.0.0.33` 与 mesh SSH 到 Mac 立即恢复，无过期缓存路由（分支3 Cacheable=false 生效）。
  - **高 RTT/丢包链路实测**（QG eth1 netem）：`delay 80ms loss 15%` 下 VM 侧 srtt 37ms→327ms、jitter 0.2ms→287ms（rttSampler 劣化跟踪正常）；**15% 丢包 lossRate 恒 0**——ping/pong 走可靠 TCP 传输，底层重传隐藏丢包，应用层 lossRate 仅反映传输无法掩盖的丢失（链路死亡级），符合 §4.3 设计预期。`loss 100%` 下完整验证 §4.4 故障检测链：lossRate 0→0.5000（10 个超时 ping 入滑窗，20 窗=50%）→ `link dead: 10 consecutive pings without pong` → 主动 close → 删 LinkNIC/注销 peer → 自动重连。**注**：该次测试 netem 黑洞同时切断了 QG 管理通道（回包同走 eth1），需人工重启 QG 恢复；教训已记录——部署环境禁止网络层黑洞类实验，劣化模拟只在本地隔离环境做。
  - **QG 重启恢复验证**：重启后 netem 配置清除（pfifo_fast 恢复），隧道自动重建（samples 重置 3，srtt 回 37ms），mesh DNS + mesh SSH 全通。
  - **问题8 结论降级为"静态已验证"**：p2p.go:1561-1587 强制版本检查 + Warn + 断开代码路径确认，但观察窗内 jf/ms9/ms10 未与 VM/QG 建立连接（无 `protocol version mismatch` 日志，亦无 v9 节点 LinkNIC），无真实 v9 触发场景，上线后观察。
