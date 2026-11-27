# MS 域名访问 / 拓扑多链路 / 代理页 P2P 状态（2026-11-27）

**创建时间**: 2026-11-27
**状态**: 问题 1 未复现（复盘见下）；问题 2/3 代码已进 v108，问题 3 的刷新路径已在 `cc2bea0` 收敛；v114（`5b2f191`）把 P2P 展示收敛为弹层并已在 vm 验证
**优先级**: 高（影响可用性认知与页面可信度）

三个独立问题，分别跟踪。

## 发版/扩散状态（2026-11-27 晚）

v108（`v0.8.3-108-gd7fd903`，含 `b890e43` 多链路、`955e919`+`d7fd903` P2P 展示修复）
扩散结果（19:45 完成）：

| 节点 | 版本 | 备注 |
|---|---|---|
| vm / qg / jf / gg / ms9 / ms10 / qgw / deskll / ylaig / ylaio | v0.8.3-108-gd7fd903 | succeeded |
| ylnas | v0.8.3-108-gd7fd903 | 上传被 507 拒，走内建 SSH 写包 + `upgrade-request` 绕过（同 v105） |
| mb | v0.8.3-108-gd7fd903 | 升级前 phaethon 已死（18:54 起无进程），走 mb 的 OS SSH（`cmll@192.168.1.88`，经 qg 跳板，`sudo nohup`）拉起 v105 watchdog 后再走正常 upload+activate |
| deskw / deskzq | — | 离线 |

多链路现场验证：jf / gg / vm 三个视角的拓扑中 `deskll↔jf` 均为 4 条 link
（2 条代理 × 双向各登记一条），不再是 1 条。

mb 掉线原因（2026-11-27）：18:03 升到 v105 成功；18:51/18:52 两次 watchdog 因
`data/instance.lock` 属主为 root、以 cmll 运行被 `permission denied` 挡住（worker exit=3）；
18:54 拉起后运行正常，但日志无退出记录，进程被外部终止（未用 nohup/launchd，绑定在启动它的会话上）。
本次用 `sudo nohup ./phaethon` 拉起，已回到拓扑（gg/qg/qgw 三条边），worker v108。
**隐患**：mb 没有 launchd 自启，注销/重启后仍会掉线，需要单独决定是否加 LaunchDaemon。

上一轮 v105（`v0.8.3-105-g0ade24d`）扩散结果：

| 节点 | 版本 | 状态 |
|---|---|---|
| qg / jf / gg / mb / ms9 / ms10 / vm / qgw / deskll / ylaig / ylaio / ylnas | v0.8.3-105-g0ade24d | succeeded |
| deskw | — | 不可达（不在拓扑中） |
| deskzq | — | 不可达（不在拓扑中） |

即：**在线 12 个节点已全部到 v105**，未完成的只有离线的 deskw / deskzq。
另外，问题 2 的拓扑多链路修复（`b890e43`）和问题 3 的页面修复都还没进任何构建产物，
所以现场仍表现为“只显示一条链路 / 状态不联动”，需要打新版本后才会生效。


---

## 问题 1：MS9 / MS10 通过域名无法访问浏览器页面

### 现象

- 用户从浏览器通过域名访问 ms9 / ms10 上的页面失败。
- 直接访问 LAN IP 也不通（见下）。

### 已查事实（2026-11-27）

- ms9 advertise：`[{"cidr":"10.161.88.0/24","id":1}]`
- ms10 advertise：`[{"cidr":"10.161.88.0/24","id":1}]`
- ms10 之前误声明为 `10.181.88.0/24`（与其实际 LAN 10.161.88.0/24 不符），已于本次排查中改回 `10.161.88.0/24`。
- ms9 / ms10 的 LAN 服务监听在 `4002-4004`，80/443 上没有 Web 服务。
- 从 vm 访问 `https://ms10.phn` 返回 200（控制台本身可达），说明 mesh 控制面与 ms10 节点本身正常。

### 复盘（2026-11-27 晚，全量 v108 后无法复现）

先排除“MTU 修复只发了 gg”：

- v105（`0ade24d`，含 LinkNIC egress 内层预算）在 17:38 已到 ms9，与 gg 是同一构建；
- 全网只有 jf↔gg 一条 UDP leg，ms9 的路径是 gg↔ms9（TCP），不经过该 leg；
- 失败当时从 vm 访问 `https://ms10.phn` 就是 200，ms9/ms10 同构建。

所以“只发 gg 导致 ms9 大包分片被丢”解释不了 ms9 的失败。

更符合现象的解释（无直接证据，保留为待证）：

1. `<nodeid>.phn` 解析出的不是 LAN 前缀，而是 `DeriveVIPFromSubnet(peer.Subnet)`（`mesh/mesh.go:2008`）。本地一旦保留过期 PeerInfo（旧 Subnet/Claims），就会解析到过期 VIP，请求落空 → 超时；这能解释“ms9.phn 超时、ms9 本身健康、从 vm 又能访问”。
2. v108 的 `b890e43` 把残留 PeerInfo 清理改为按链路身份判定，过期条目不再遮蔽实时 Subnet/Claims；同时 ms10 的 advertise 已统一为 `10.161.88.0/24`。全量统一后现场复测 200。
3. 曾记录的 ms10 `10.181.88.0/24` 属现场配置记录（无原始 API 快照），且该错误前缀本身解释不了 ms9 的失败，只作为 claims 不一致的旁证。

结论：不是单点配置错误，也不能归因于“MTU 只发 gg”；最可能是过期拓扑/claims 造成的 VIP 解析与选路偶发错误，随 v108 统一与残留清理修复而消失。当前未复现，不再为此追加代码改动。

### 待办

- [x] 用真实 URL 从 vm / deskll 复测 ms9、ms10 页面。
      2026-11-27 19:50 从工作站（mesh 内）复测：`https://ms9.phn/`、`https://ms10.phn/` 均 200；
      `10.161.88.9/10` 的 `4002-4004` 均能建 TCP 并返回应用响应（根路径 404 属正常），
      ICMP 68-72ms 0% 丢包。仍待用户提供实际报错的业务域名/URL 才能复现。
- [ ] 确认 10.161.88.0/24 多 owner 下的选路结果与回程路径。

---

## 问题 2：deskll 配置两条到 jf 的代理，拓扑图只显示一条

### 现象

deskll 有两条 H_TUNNEL 代理指向 jf（`10.140.51.2`）：

| 代理名 | URL |
|---|---|
| `DESK_TJ (copy)` | `http://10.140.51.2:32457/ui/resdata/` |
| `DESK_TJ_WS` | `http://10.140.51.2:8080/ui/resdata/` |

两条均 `p2p=true, udp=true`，但 mesh 拓扑图里 deskll↔jf 只有一条边/一条链路。

### 根因（已定位）

**数据结构层面只保留一条，不是前端展示问题。**

`mesh/topology.go:228 RegisterPeer` 在 `2030dc1`（fix(admin): mesh 页面路由编辑/删除错位…）中加入了按 nodeID 去重：

```go
if id := sender.GetNodeID(); id != "" {
    kept := t.peers[:0]
    for _, p := range t.peers {
        if p.Sender != sender && p.NodeID() == id {
            continue // 丢弃同 nodeID 的旧条目
        }
        kept = append(kept, p)
    }
    t.peers = kept
}
```

该去重本意是清理重连后残留的旧 PeerInfo（残留条目会用过期 ClaimedSubnets/Neighbors 遮蔽实时数据，表现为拓扑边永久缺失）。副作用是：**同一节点经第二条代理再建链时，先注册的 PeerInfo 被删除**，因此本地拓扑只剩一条 Sender，`GetFullTopology` 里每条边只能拿到一个 link。

代码里其它位置（`mesh.go:2526/2740/3384`、`p2p/link_quality_test.go:143`）明确按“同一 node 多条链路”设计（多条 Peer 共享 nodeID、按 localSeq/remoteSeq 分组），与 `RegisterPeer` 的去重冲突。

### 修复方向

去重条件从“同 nodeID 即删”收紧为“同 nodeID 且同链路身份（proxy/Seq）才删”，或仅在重连场景（同 proxy 名）删除残留条目。保留真正的多链路 PeerInfo。

### 修复（2026-11-27，代码已改）

`mesh/topology.go` 新增 `linkIdentity(sender)`：

- 出向链路用 `proxy:<代理名>` 作身份（同一代理重拨 = 同一链路，旧条目清掉）；
- 没有代理名的入向链路用 `seq:<localSeq>/<remoteSeq>` 作身份。

`RegisterPeer` 的残留清理条件改为 **同 nodeID 且同链路身份**，因此同一节点经第二条代理建链时
旧条目不会再被误删；`GetFullTopology` 本来就按 PeerInfo 逐条追加 link，边上的 `links` 会自然变成 2。

单测 `mesh/topology_register_test.go`：同节点两条不同代理 → 2 条；同代理重连 → 替换旧条目（仍 2 条）；
两条无代理入向链路 → 2 条；不同节点互不影响。

遗留说明：入向链路（无代理名）如果漏掉 `UnregisterPeer`，残留条目靠 seq 不同不会被清掉，
只等清理事件到达；这与本次多链路修复不冲突，若后续出现再单独处理。

### 待办

- [x] 设计链路身份（proxyName 或 seq 对）判定，避免误删多链路。
- [x] 补单测：同一 node 两条 proxy 注册后 `GetAllPeers` 长度 = 2（`GetFullTopology` 按条追加 link）。
- [ ] 现场验证：deskll 拓扑上 deskll↔jf 应出现两条链路。
- [ ] 复核 `GetAllPeers` 排序、`bestClaims` 在多链路下不重复计入。

---

## 问题 3：deskll 代理页 P2P 状态展示不稳定、与开关不联动

### 现象

- P2P 状态时而显示已连接、时而回到 connecting。
- 与代理开关（p2p 开关、UDP Direct、V2/WS 勾选）状态不一致，无法从页面判断实际生效的是 TCP 还是 UDP Leg、WS 是否开启。

### 已查事实

`admin/templates/proxies.html`：

- 表格首屏 P2P 单元格由服务端模板 `.IsP2P`（配置项）渲染，永远是“connecting”或“disabled”，不代表运行时状态。
- 运行时状态只靠 `window.fetchP2PStatus()` 拉 `/api/p2p` 覆盖单元格（`proxies.html:1920-1996`）。
- `fetchP2PStatus` 只在页面加载、proxies 列表刷新、以及 SSE `p2p` 业务版本事件时调用（`app.js:670-675`）；**没有轮询、没有 SSE 重连后的补偿刷新**。一旦漏掉一次事件（SSE 断线、事件合并），单元格会长期停在上一次的旧状态。
- 单元格只展示一个 `transport` 徽标（`udp` / `tcp` / `tcp+stripe×N`），UDP Direct 勾选、V2/WS 是否启用都不在状态单元格中体现；`p2p-udp` 勾选与 `transport` 是两回事，容易读成“开关没生效”。

### 修复（2026-11-27，已收敛到统一版本通知机制）

- 单元格双维度渲染：配置态（`p2p` / `p2p-udp` / `v2`）+ 运行时快照（`/api/p2p`）；
  transport 徽标只在连接建立后显示，区分 UDP / TCP / tcp+stripe×N。
- 刷新只走 `VersionNotificationService` 的 `p2p` topic：
  - 页面不再有独立定时器、不再监听 `visibilitychange`、开关操作后不再直刷；
  - SSE 重连/首次打开时拉版本向量并按 topic 差量重放（`fetchVersions` → `dispatchAll`），传输层不含页面函数特判；
  - 服务端在 `StopPeerByProxyName` / `StopPeerByNodeSeq` / `StopAllPeers` 断链后自增 p2p 版本，开关与断链都能驱动刷新；
  - 启动时对 `p2p` topic 做一次种子自增，保证版本向量从进程启动就带该 topic，页面首次打开也能通过重放拿到快照。
- 结论：页面已无任何独立轮询/补偿刷新，问题 3 的“刷新绕开版本通知”已纠正（`cc2bea0` + 本次种子自增）。

### 验证（2026-11-27 21:0x，vm 已升级到 `v0.8.3-112-g07aa4b3`）

headless 打开 `https://127.0.0.1:39999/proxies` 实测：

- 首屏 `/api/p2p` 只请求 1 次，由 SSE `p2p` 版本重放触发（页面无直拉）；
- 空闲 17s 额外请求 0 次（无轮询）；
- `visibilitychange` hidden→visible 额外请求 0 次（无直刷）；
- 单元格正确区分生效态：`AIOPS_TJ`/`YLAIG_TJ` = connected + TCP + `V2`，
  `QG_SS5_UDPLEG` = connected + UDP + `UDP`；无 pageerror。

### 交互收敛（v114 `5b2f191`，2026-11-27 21:3x）

代理表格由 13 列收敛到 11 列，运行时链路信息全部收进可点击弹层：

- 表格删除独立的 V2 / 承载列；P2P 单元格只保留一个入口（兼容类型可点，非兼容类型显示灰色 `—`，无弹层）。
- 弹层内按“配置态 + 运行时态”分组呈现：P2P 主开关、UDP Direct、V2、UDP Leg、WebSocket 等并列/级联关系一次可见。
- P2P 主开关关闭时子选项仍可见但置灰，已存值保留不清空（对应约束 2）。
- UDP Direct 已配置但回退 TCP 时显示琥珀色警示徽标，而非绿色（对应约束 3）。
- h_tunnel 类型 P2P 强制开启，编辑表单中主开关置灰且不可关闭（对应约束 4）。
- 新增 `PATCH /api/proxies/{name}/options` 增量接口，仅修改请求体里出现的 `udp/p2p/p2p-udp/v2` 字段，避免整表单回写丢失 `uuid/reality-opts` 等字段；旧的 `/p2p` 独立端点移除。
- 弹层变更后调用 `refreshP2PCell()` 立即重绘单个单元格，不等下一次版本事件；表格体因版本事件重建时 `refreshP2PPopover()` 重新锚定弹层，不打断用户操作。刷新仍全部走 `VersionNotificationService`，无新增页面级定时器。

vm 验证（`v0.8.3-114-g5b2f191`，watchdog 自升级，current/requested 均 v114、succeeded）：

- `/api/proxies` 返回 `p2p-udp-configured` / `v2-configured` / `carrier` 字段；
- `/proxies` 页面 200，含 `proxy-table` 与 `p2p-popover` 标记；
- `PATCH .../options` 幂等成功（`GGDYSV_TJ` 置 `p2p=false`，`v2-configured` 保留为 true）；
- 不存在的代理返回 404；对 h_tunnel 关闭 P2P 返回 400 并给出原因。

### 首屏状态时序修复（v116 `f7a0b54`，2026-11-27 22:0x）

vm 实测发现单元格/弹层在页面加载后约 4 秒内仍显示“连接中”，而 `/api/p2p` 其实已有 `upToDate`：

- 首屏 0.65s 就渲染表格，但第一次 `/api/p2p` 直到 **4.77s** 才发出（由 4.76s 的 `/api/versions` 触发）。
- 根因：`util/version.go` 的 `Subscribe` 建连时不写任何字节；Go `net/http` 不 flush 响应头，浏览器 `EventSource.onopen` 要等第一个周期性心跳/版本 bump 才触发（心跳间隔 10s），于是版本重放与 `fetchVersions` 被整体推迟，首屏按配置画的 `connecting` 停留数秒。
- 修复：`Subscribe` 注册后立即向该连接推送一帧当前完整版本向量（`sendHeartbeat`），既 flush 了响应头让 `onopen` 立刻触发，也让客户端即时种下版本向量。仍全部走版本通知机制，无页面定时器。新增单测 `TestVersionNotifier_SubscribePushesSnapshotImmediately`。

vm 复测（`v0.8.3-116-gf7a0b54`）：

- 第一次 `/api/p2p` 从 4.77s 提前到 **0.70s**；表格首次采样（0.82s）即 `AIOPS_TJ/QG_SS5_UDPLEG/YLAIG_TJ` 全部 `connected/upToDate`；
- 弹层状态正确：`AIOPS_TJ`=已连接/TCP、`QG_SS5_UDPLEG`=已连接/UDP、`YLAIG_TJ`=已连接/TCP、`GGDYSV_TJ`（p2p 关）=已关闭、`QG_HT` 主开关置灰并显示 `WS` 承载、`LAOYU_SSH_TEST`（ssh）无按钮只有 `–`；
- 可操作：弹层内 `V2` 勾选关闭/恢复各触发一次 `PATCH .../options`（均 200），复选框、单元格 `v2` 徽标与 `v2-configured` 即时联动并成功还原，无 console/pageerror。

### h_tunnel 放开 P2P + 收敛 link 字段（2026-11-27 晚）

- 移除 `Proxy.IsP2P()` 对 h_tunnel 恒返回 true 的强制约束，`PATCH .../options` 不再拒绝关闭；h_tunnel 关闭 P2P 后走 v1 HTTP tunnel 握手直连代理服务器。
- 编辑表单 / popover / copyProxy 去掉 h_tunnel 的置灰与 🔒，所有类型行为一致。
- `/api/proxies` 只出口原始配置值：删除 `p2p-udp-configured` / `v2-configured` 派生字段，`p2p-udp` / `v2` 直接返回存储值；生效值由前端按 `p2p && p2p-udp`、`p2p && v2` 推导，保持平铺结构，不做 schema 迁移。
- `TestIsP2P_HTunnelCanBeDisabled` 覆盖“显式 false 可关闭 / 未设置默认开启”。

### 站内导航后 P2P 圆点常闪（2026-11-27 晚）

mb 实测：从其它页面点侧边栏进入代理页后，GG_TJ 圆点一直蓝色脉冲，点开弹层状态却可能已是“已连接”。

- 根因：`/proxies` 服务端渲染的 P2P 单元格是固定的 `connecting` 占位（运行时状态只存在于 `/api/proxies`）；HTMX 换页不触发 `DOMContentLoaded`，而 `proxies` / `p2p` 版本已同步时版本机制不会重新 dispatch，占位因此永不重绘。弹层读同一份全局数据，若此前版本事件刷新过全局，就会出现“圆点闪、弹层已连接”的割裂。
- 修复：`htmx:afterSettle` 检测到 `#proxy-table` 时主动调用一次 `fetchProxies()`（仍走同一快照，不加定时器）。
- Playwright 复现：HTMX 导航后 13s 圆点仍为 `p2p-status connecting`、`runtime=null`；修复后应立即变为 `connected/upToDate`。

### 待办

- [x] 统一 P2P 单元格数据源为运行时状态 + 配置态两个维度。
- [x] 刷新回归 `p2p` 版本事件驱动（无独立轮询）。
- [x] 表格收敛为 11 列，P2P 配置/运行时状态收进弹层（v114）。
- [x] 首屏状态不等心跳，SSE 建连即推送版本快照（v116）。
- [x] h_tunnel 允许关闭 P2P；API 删除派生字段，仅出口原始配置值。
- [x] HTMX 站内导航进入代理页后主动拉取一次快照，修复圆点常闪。
- [ ] 扩散到其余节点后复测代理开关、UDP Direct、V2/WS 勾选后的展示一致性。

