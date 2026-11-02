# StallMonitor 精准 Dump 阶梯

## Context

`mesh/stall_monitor.go` 三条规则触发即写 445KB 全量 goroutine dump（`writeDump`）。2026-11-02 dump `goroutine-dump-20261102-135925.log` 证明 rule 0 可以合法触发（D 窗口流动 212≥60 后冻结 90s+），但 dump 内容只回答了"VM 内部 goroutine 全健康"，**定位嫌疑仍需人工分析**——dump 前没有任何"是哪一环死了"的判态。

目标：rule 触发后先**廉价判态**（KB 级日志），**确认 phaethon 侧嫌疑后才写全量 dump**。host-silent / 死链自愈等场景直接跳过 dump。

## 目标

1. rule 触发 → L1 快照行（JSON 单行，KB 级）+ 三判态
2. 判态指向 phaethon 侧嫌疑 → L2 主动探测（in-stack socket，两条腿）确认
3. 探测确认 → 才写全量 dump（L3）；否则只留快照+探测结果日志
4. 现有 rule 评估 / arming / freezeWindow / minTargetActivity 门控**全部保留不动**
5. manual `TriggerDump` 行为不变（任何时候都写全量 dump）

## 设计

### 阶梯总览

```
rule fires (现有逻辑不变)
  │
  ▼
L1 快照行 ── disposition ──┬─ dead-link-suspect ──→ L2 mesh 腿确认 ──→ L3 dump
  (KB 级日志)              ├─ ping-healthy ───────→ L2 双腿探测 ────→ L3 dump (探测指向 phaethon 侧)
                           ├─ host-silent 确认 ────→ 只留日志，不 dump
                           └─ ambiguous ──────────→ L2 双腿探测 ────→ 按矩阵判
```

### L1：快照 + 三判态

`fire()` 里 `writeDump` 之前，先写一行 JSON（`util.LogModuleWarn("mesh", ...)`）：

```json
{"event":"stall-snapshot","rule":0,"counters":{"A":..,"B":..,"Dvip":..,"Dipip":..,"samples":[12]},
 "peers":[{"node":"qg","srttMs":12,"jitterMs":3,"lossRate":0.01,"consecutiveLost":0,"sampleCount":4021,"lastSeenAgeMs":800}],
 "linkNics":[{"peer":"qg","injectDelta":179,"injectDrops":0}],
 "disposition":"ping-healthy"}
```

disposition 判态（纯函数，可单测）：

| 判态 | 条件 | 去向 |
|---|---|---|
| `dead-link-suspect` | 任一 peer `consecutiveLost >= 5`（阈值 10 即自动关链，5 是预警带）或窗口内 `lossRate > 0.3` | L2 mesh 腿 |
| `host-silent-suspect` | rule==2 且所有 peer ping 健康 | L2 host 腿 |
| `ping-healthy` | rule 触发但所有 peer ping 完全健康（link 数据面活着） | L2 双腿（嫌疑在 phaethon 内部或上游） |
| `ambiguous` | 无 peer / 其他 | L2 双腿 |

数据全部来自现有基础设施，零新机制：
- 计数器/样本：`StallMonitor` 现有字段
- per-peer pingTracker：`p2p.go:1928 getStats()` 返回 (srtt, rto, jitter, lossRate, sampleCount)，`consecutiveLostCount()` 在 `p2p.go:1904`
- 逐 LinkNIC inject 增量：`mesh.go:2682 GetLinkNICStats()` → `link_nic.go:209 GetStatistics()`（injectPackets/injectBytes/injectDrops），L1 采样时取一次，rule arming 时取基线算 delta

### L2：主动探测（两条腿，socket 来源不同）

普通 socket 探测，**不用 raw SYN 注入、不用双端口矩阵、不用栈快照兜底**。patch #15（6739c92）修复后普通 socket 经 LinkNIC 出向已可用，此设计直接受益。

探测在独立 goroutine 跑，每腿超时 3s，**不拿任何业务锁**，结果只写日志。

**Mesh 腿——gvisor in-stack socket → 邻居 GIP:53**（`Netstack.ProbeMeshPeerDNS(gip net.IP, timeout)`，`mesh/netstack.go`）：

复现 `ResolveDomainViaDNS` 的 socket 序列：NewEndpoint(udp)+Bind({})+Connect(GIP:53)+Write(BuildDNSQuery(...))，waiter 等响应，收到**任何** UDP 响应即判活（不校验 DNS 语义——对端 DNSHijacker 在 GIP:53 必应答）。对 rule 涉及的所有 mesh peer 的 GIP（`GetPeers` → peer subnet → `DeriveGIPFromSubnet` .3）并行探测。

**Host 腿（rule 2 专用；RunDiagnostics 自检也跑）——宿主 socket → 自己 GIP:53**（包级函数 `probeHostViaOwnGIP`，`mesh/stall_monitor.go`）：

```go
conn, _ := net.DialUDP("udp4", nil, &net.UDPAddr{IP: ownGIP, Port: 53})
conn.SetDeadline(now.Add(timeout))
conn.Write(BuildDNSQuery("probe.phn", txid))
conn.Read(buf) // 任何应答 = 环全活
```

关键在 socket 来源：这是 **Go 宿主协议栈的 socket**（不是 gvisor in-stack），目标是自己 GIP。查询包从宿主出发 → 经 wintun 进 netstack（计数器 A 同路径）→ 本地 DNSHijacker 应答 → 应答经 netstack 写回 wintun（计数器 B 同路径）→ 宿主收到。**收到任何应答 = rule 2 怀疑的每一环（宿主协议栈 / wintun 双向 / netstack 读写）物理上全活**——零假阳性；超时 → dump。

这一跳天然横穿 TUN 两次，一条查询覆盖全部嫌疑环，所以**不需要栈快照兜底**：超时在所有子情形下（宿主栈死 / TUN 断 / netstack 不回写）都该 dump，没有"探针无响应但环其实活"的歧义分支。

**为什么 host 腿不能用 gvisor in-stack socket 访问 hostIP**（2026-11-02 代码核实）：
- `route_selector.go:91-101` branch 1：dst=hostIP(.2) 在 LocalSubnet 内 → `LocalStack = (dst != VIP)` = true（.2 非 VIP）
- gvisor-fork `stack.go:1663-1668`（patch #12）：FindRoute 对 LocalStack 目标 `r.setLocalStack()` → in-stack 回环路由，包不出 NIC 1 进 TUN，netstack 内部本地交付（无 socket 绑 .2，直接丢弃）
- 这是设计语义而非缺陷：netstack 视角 .2 是"宿主侧地址"，不是"经 TUN 可达的邻居"
- 推论：host 腿必须走**宿主 socket**（天然出 TUN），而 mesh 腿必须走 **gvisor in-stack socket**（peer GIP 走 branch 2 → LinkNIC，且 ResolveDomainViaDNS 生产路径已在 QG 验证可用）——两腿 socket 来源不同是设计要求，不是巧合

**旧方案废弃原因**：
- **39999 TCP connect**：39999 是配置项（改配即假阴性）；"Phaethon Admin" 防火墙规则只在本机核实过，其他部署无此规则
- **TUN 直注 SYN + readA 正向信号 + 栈快照兜底**：依赖 Windows 防火墙语义（RST 不回即落兜底），栈快照按函数名定位 readLoop 脆弱且实现重；宿主 socket 方案一条查询给出确定性答案，全部不需要
- **hostIP .2 经 gvisor 放行**：in-stack 访问 .2 结构性不通（见上），作为备选方案记录在案（若未来要做，2 行改动镜像 VIP），本次不做
- **旧 `probeHostLiveness`（raw SYN 注入 + buildSYN checksum）整体删除**——生产零运行记录、被 host 腿完全覆盖。`TestBuildSYNChecksums` 一并删除

**解释矩阵**：

| 场景 | 结论 | 动作 |
|---|---|---|
| mesh 腿任一有响应 | mesh 数据面端到端活（含回程），嫌疑 phaethon 内部/对端上游出口 | L3 dump |
| mesh 腿全无响应 + pingTracker 高丢包 | 死链确认 | L3 dump |
| rule 2：host 腿有应答 | 宿主栈 + wintun 双向 + netstack 读写全活 → host-silent 良性 | **不 dump** |
| rule 2：host 腿超时 | 环上至少一环真死（宿主栈 / TUN / netstack） | L3 dump |

### L3：确认后全量 dump

仅当矩阵判定"嫌疑 phaethon 侧"才调用现有 `writeDump`。dump header 增加 L1 快照行 + L2 探测结果（复用同一 JSON，便于取证关联）。

### 时序核实（已确认）

- ping 发送间隔 1s（`p2p.go:1502`），`pingFailDisconnectThreshold=10`（`p2p.go:1783`）→ 死链 ~10s 内被 pingTracker 自判并自动关链触发路由重算
- freezeWindow=60s → rule 触发时 L1 快照里的 `consecutiveLost` 必为新鲜证据（不存在"链路早死但计数 stale"窗口）
- 死链已被 pingTracker 自动关链 + 最优路径重算切活链 → 数据面自愈，此类 dump 价值低；disposition=`dead-link-suspect` 且探测不通时仍 dump（留证据链），但在快照行标注 `self-heal-likely=true`（consecutiveLost≥10 即已触发自动关链）

### Admin 自检端点（部署后验证探针好使）

`admin/admin.go` 新增 `POST /debug/stall-probe`（mTLS 已有）：同步执行 L1 快照 + L2 双腿探测（`StallMonitor.RunDiagnostics`），返回 JSON 结果，**不写 dump**。

部署 VM 后实测一次：mesh 腿应返回通（qg 链路活着），host 腿应返回通（宿主 socket 查自己 GIP，本地 DNSHijacker 必应答）——确认两条腿在生产 socket 路径上工作正常，替代"本地环回测试"的局限。

### Manual dump

`TriggerDump(manual)` 现有行为不变：直接写全量 dump，不走阶梯。

## 实现清单

| 文件 | 改动 |
|---|---|
| `mesh/stall_monitor.go` | `fire()` 接阶梯：L1 快照行 → disposition → L2 探测 → 条件 L3。新增 `probeHostViaOwnGIP`（宿主 socket → 自己 GIP:53）。删除 `probeHostLiveness`/`buildSYN`/`ipv4Checksum` 等。rule 评估/arming/门控零改动 |
| `mesh/netstack.go` | 新增 `ProbeMeshPeerDNS(gip, timeout)`（gvisor in-stack socket → 邻居 GIP:53） |
| `p2p/p2p.go` | 新增 `GetLinkConsecutiveLostByNodeSeq`（pingTracker consecutiveLost 访问器） |
| `mesh/mesh.go` | `P2PTransport` 接口加 consecutiveLost 访问器；`GetLinkConsecutiveLost`、`GetLinkNICStats` 真实实现（接线） |
| `admin/admin.go` + `main.go` | `POST /debug/stall-probe` 自检端点（`RunDiagnostics`） |
| `mesh/stall_probe_test.go`（新） | `ProbeMeshPeerDNS` 活/死单测（活=真本地 hijacker 应答，死=fakePeerSender 不应答超时）；`probeHostViaOwnGIP` TEST-NET 超时单测；disposition 纯函数单测；rule 2 host 腿活跳过 dump / 死写 dump；rule 0 恒 dump |
| `mesh/stall_monitor_test.go` | 删除 `TestBuildSYNChecksums` |

## 约束

- 现有执行逻辑（rule 评估、arming、freezeWindow、minTargetActivity、计数器采样）一行不动，只改 `fire()` 之后的路径
- 探测 goroutine 独立 + 超时 + 不持业务锁，探测失败绝不影响主流程
- 所有新日志走 `util.LogModuleWarn("mesh", ...)`，与现有 stall 日志同级
- 版本戳构建 + 部署顺序 VM → 用户确认 → QG；QGW 只放二进制不重启

## 验证

1. 单测全绿：`go test ./mesh/ -vet=off`（2026-11-02 已全量通过）
2. VM 部署后 `POST /debug/stall-probe`：两腿均通（预期）
3. 复算 135925 场景：rule 0 + qg 死链 → L1 判 `dead-link-suspect` → mesh 腿不通 → dump，快照行直接给出"qg 链路死"结论（vs 现在需人工分析 445KB）
4. 回归：正常负载 24h 不产生新 dump、admin 巡检无异常
