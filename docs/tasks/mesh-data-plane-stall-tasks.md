# Mesh 数据面卡死排查任务（p2p 写路径 / TUN 读循环 / gVisor 握手）

关联问题记录：`docs/issues/mesh-data-plane-stall.md`

## 目标

定位 VM↔QG 数据面双向卡死（`CreateEndpoint` 永久挂起 >20 分钟）的根因，并按审查结论修复。约束：现场仍在（VM 留跑 226），争取 pprof 抓挂死栈实锤。

## 阶段一：静态代码审查（只读，零风险）——已完成 2026-11-01

结论已回填 `docs/issues/mesh-data-plane-stall.md` 根因链条：
- [x] 嫌疑点 1（p2p 写路径）：`enqueueWrite` 非阻塞不背锅；实锤在 transport 层 —— `meshChannelTransport.Send` 无超时 + 心跳无限续命 → peerWriteLoop 永久卡死
- [x] 嫌疑点 2（重复会话）：`Topology.RegisterPeer` 无去重 + `SendToNode` 同跳 round-robin → 流量摊到死活两条会话；failover 因 Send 入队语义永不触发
- [x] 嫌疑点 3（gVisor 握手）：`retransmitHandlerLocked` 被动握手无 maxRetries 上限 → CreateEndpoint 永不返回（定时器正常，设计上不放弃）
- [x] 交叉核对：与全部症状自洽（单向卡死、偶发能通、控制面正常、干净重启复现）

## 阶段二：可观测性 —— pprof/goroutine dump（代码变更，须用户确认后动手）

**核心约束：断网后必须仍能拿到 dump**。设计三条互备路径：

- [x] **路径 1（轻症）**：admin API 挂 pprof 标准端点（`/debug/pprof/`，goroutine/heap/allocs）。
  - 本机即 VM：mesh 断流不影响 localhost，操作时 `curl 127.0.0.1:39999/debug/pprof/goroutine?debug=2`
  - 复用现有 admin 监听与认证，不新增端口
  - 引入 `net/http/pprof` 即可（注意：须挂载到 admin 的 mux 而非默认 DefaultServeMux，除非 admin 已用默认 mux）
- [x] **路径 2（重症）**：进程内停滞监测 + 自动 dump。判定逻辑（基于 atomic 计数器，不侵入阻塞调用）：
  - 计数器：A=TUN readPackets、B=netstack WritePackets、D=入向帧中"本机 host-facing 前置"帧数（新增，挂 HandleMeshFrame）；全部本机维度，转发帧不计入（用户指正：framesSent 含纯转发流量，与 TUN 无必然关系，弃用）
  - **D 的精确定义（2026-11-01 两轮质疑后收敛，依据 route_selector.go:85-161 + fork channel.go:203 核实）**：D 只数两类帧——
    - **目的==本机 VIP(.1)**：分支1 LocalStack=false，"a packet still addressed to VIP must leave NIC 1" → 出 TUN。这是本机 host 出站流量（SNAT src=VIP）的回程
    - **外层目的==本机 EIP(.4) 的 IPIP 帧**：VM 走 QG 出口的回程形态（外层不进 TUN——用户指正，原表述"local advertised→NIC1→TUN"是过度推断，撤回——但帧的到达是 B 的前置：decap→内层交付本机 forwarder→回包写 TUN）
  - **明确不计入**：GIP(.3)/hostIP(.2)/fakeIP 的 plain 帧（对端访问本机 DNS/forwarder，栈内闭环，回包走 Link NIC，与 TUN 无关）；他节点/IPIP 中继转发帧；local advertised 网段（VM 是端点不承接，网关角色将来需细化，记录在案）
  - 实现：HandleMeshFrame 里判定（`m.subnet` 算 VIP/EIP 常量比较 + `frame[9]==4` 判 IPIP），无需引 RouteSelector；MeshManager 已有全部上下文
  - **注入侧无队列（用户质疑②核实的正面结论）**：fork `channel.go:203` InjectInbound 直投递，入向不存在"注入队列满丢"；A 之后、B 之前的静默丢包发生在 gVisor 栈内部（无路由/校验/handleValidatedPacket 丢弃）
  - **设计原理：每条规则 = 一条健康时必然成立的因果承诺被破坏**：
    - 承诺1（应答）：A 涨 → D 必涨（本机在发，mesh 目的的回程必以 VIP 或 IPIP-EIP 形态抵达）
    - 承诺2（投递）：D 涨 → B 必涨（上述两类帧的到达必然产生 TUN 写：VIP 帧直出 NIC1；IPIP-EIP 帧 decap→本地交付→回包写 TUN）
    - 承诺3（读取）：B 涨 → A 必涨（回包持续写进 TUN，本机应用必有后续发包）
  - **规则1**：A 涨、D 冻 60s → 只出不进（本案轻症 VM 侧表象；含栈内静默丢弃——归因靠 dump 内 Stats 快照，见下）
  - **规则2**：D 涨、B 冻 60s → 投递链卡死（decap/本地交付/writeLoop/TUN 写任一环节）
  - **规则3**：B 涨、A 冻 60s → 触发取证（12:09 重症形态：写侧照涨、读侧全冻）
  - **规则3 归因闭环（质疑③：栈无法区分"钉死"与"正常等待"——`device.Read` 上两种状态栈文本相同）**：触发时 monitor 主动探活——经 writeLoop 向 TUN 写入目的=任一对端 VIP 的 TCP SYN，host OS 按 /1 路由将其送回 TUN，读循环健康则 5s 内 A 必 +1（随后走正常 mesh 路径到对端，对端回 RST，无害）。A 动 → 主机沉默（误判，dump 标注 probe=responded）；A 不动 → 读循环真死实锤。前提自洽：规则3 前提即 B 涨（writeLoop 活着，探针必被写进 TUN）
  - **规则3 已知的逻辑漏洞（如实记录）**：单向 UDP 流（下载类）健康状态下 host 协议栈不产生自动回包，B 涨 A 天然冻——规则3 对此会误触发；探活会将其正确归因为"活着"，代价只是一次 dump
  - **误判零副作用（确认记录）**：monitor 完整行为 = sleep 10s + atomic.Load ×3 + 触发时写一次文件；不拿业务锁、不进数据路径、不发业务网络包、不干预连接/重连。唯一全局影响是 `runtime.Stack(buf, true)` 的毫秒级 STW，每事件一次
  - monitor goroutine 每 10s 抄数对比；触发后 dump 每个事件只一次、恢复后重新武装
  - **dump 内容（质疑②的直接回应）**：① `runtime.Stack(buf, true)` 全 goroutine 栈；② gVisor `stack.Stats()` 丢弃统计快照（IP/TCP 各丢弃计数）+ 三计数器当前值；③ 规则3 触发时附探活结果（probe=responded/timeout）——丢弃型黑洞（Stats 异常膨胀、goroutine 干净）与真卡死（Stats 干净、goroutine 钉死）一眼可分，避免冤枉成"对端不回"
  - 依据：`tun/manager.go:732` TUN readLoop 阻塞在 `device.Read`（无超时），但 `readPackets` 每读到一包 +1——卡死留下足迹（计数器冻结），独立 monitor（只 sleep + atomic.Load，不拿业务锁）从外部观测
  - 残留假设（记录在案）：① VM 是端点非中继/网关；纯中继或承接旁路网段时规则1/3 可能误报、IPIP-EIP 之外可能出现其他 host-facing 形态，将来角色变化需细化；② 规则1 区分不了"本地丢包/卡死"与"对端不回/路径断"，归因靠 dump 的 Stats 快照
  - 根本限制（记录在案）：① B 只承诺"成功写进 wintun/tun 设备"，host 侧缓冲满照样丢包——丢包劣化检测不了，但零星丢包不冻结计数器，卡死判定不受影响；② pre-A 丢包（host 应用→wintun 环满，尚未进进程）与"主机沉默"同签名，计数器天然不可见；③ 规则1 无法本地探活区分"本地丢包/卡死"与"对端不回"（对端沉默不可探测），归因只能靠 dump 的 Stats 快照；读阻塞 vs 主机沉默已由探活闭环（见上）
  - 断网可取证：monitor 在 worker 进程内，业务全卡死它也照常运行；分析在事后读文件，无需在线、无需网络
- [x] 构建本地二进制验证：curl localhost pprof 端点拿栈；构造模拟场景（停掉对端读）验证停滞判定触发 dump
- [x] 部署 VM（本机）留跑，下次卡死立即取证（2026-11-01 已部署 v0.8.0-227-g52a21da：进程对/TUN/mesh 路由/ping qg.phn 全通，pprof/dump 端点挂载且被认证保护）

## 阶段三：治本 B（触发源）+ 回归验证

- [ ] 依据阶段二 dump 定 B 的罪，修复方案另写文档讨论后动手
- [ ] 4 项修复（Send 超时 / 出站停滞检测 / 健康度选路 / 会话去重）已全部撤销，理由与风险记录见 issue 文档。唯一出路是治 B
- [ ] 压测必须包含 TUN 层（用户明确要求）：用跨平台组合 —— Windows 实例 + Linux/Mac/Docker 实例互连，各占本机一个 wintun/tun，规避单平台双实例排他性。
- [ ] 禁止：netem/tc/iptables 等网络层劣化实验（铁律）；禁止关 TUN 纯 p2p 层替代压测。
