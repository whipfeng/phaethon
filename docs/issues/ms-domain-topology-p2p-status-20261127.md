# MS 域名访问 / 拓扑多链路 / 代理页 P2P 状态（2026-11-27）

**创建时间**: 2026-11-27
**状态**: 修复中（问题 2 代码已修待发版；问题 3 代码已改待验证）
**优先级**: 高（影响可用性认知与页面可信度）

三个独立问题，分别跟踪。

## 发版/扩散状态（2026-11-27 晚）

v108（`v0.8.3-108-gd7fd903`，含 `b890e43` 多链路、`955e919`+`d7fd903` P2P 展示修复）
扩散结果（19:45 完成）：

| 节点 | 版本 | 备注 |
|---|---|---|
| vm / qg / jf / gg / ms9 / ms10 / qgw / deskll / ylaig / ylaio | v0.8.3-108-gd7fd903 | succeeded |
| ylnas | v0.8.3-108-gd7fd903 | 上传被 507 拒，走内建 SSH 写包 + `upgrade-request` 绕过（同 v105） |
| mb | — | 当前不在拓扑中，`mb.phn` 不可达（升级前已离线） |
| deskw / deskzq | — | 离线 |

多链路现场验证：jf / gg / vm 三个视角的拓扑中 `deskll↔jf` 均为 4 条 link
（2 条代理 × 双向各登记一条），不再是 1 条。

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

### 根因假设

1. **错误前缀已排除**：ms10 曾声明 `10.181.88.0/24`，导致发往 10.161.88.x 的回程/域名路由无法落到 ms10，已修正。
2. **同前缀多 owner**：ms9 与 ms10 处于同一 LAN（10.161.88.0/24），两边都声明该前缀，属多 owner 场景。入口选 egress node 时按质量加权 rendezvous 选一个 owner，选到非目标主机所在节点时依赖对端 LAN 转发，若对端到该主机的二层/三层路径不通即失败。需实测确认。
3. 页面端口本身（4002-4004）是否对外可达，需要按真实 URL 复测。

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

### 修复方向

- 状态单元格改为完全由 `/api/p2p` 驱动，并在 SSE `p2p` 事件之外增加低频轮询/重连补偿，避免漏事件后长期失真。
- 状态单元格同时显示：连接状态（connected/connecting/failed）、实际传输形态（TCP / UDP Leg / tcp+stripe）、以及 P2P/UDP Direct 配置开关是否开启，区分“配置”与“生效”。
- 开关操作后立即本地置灰/loading，并以服务端返回为准回填，避免二次渲染回退。

### 修复（2026-11-27，代码已改，待验证）

- `app.js` 新增 `renderP2PCell(proxy, info)`，统一由“配置态 (`p2p` / `p2p-udp` / `v2`) + 运行时快照 (`/api/p2p`)”渲染单元格：
  状态点 + 生效传输徽标（UDP / TCP / tcp+stripe×N）+ 配置小标签（`udp` / `v2`，仅配置开启时显示）。
  这样“勾了 UDP Direct 但实际回落到 TCP”能直接从单元格看出来。
- `proxies.html` 的 `updateP2PStatus` 改为复用该函数，消除两份渲染逻辑分叉。
- 补偿刷新：`fetchP2PStatus` 增加并发保护与 `cache:no-store`；页面在可见状态下 15s 低频轮询；
  `visibilitychange` 回到前台立即刷新；SSE `onopen`（含重连）成功后立即重新拉取。
- P2P / V2 / UDP 开关操作成功后立即重新拉取运行时状态，避免只能等 SSE。

### 待办

- [x] 统一 P2P 单元格数据源为运行时状态 + 配置态两个维度。
- [x] 增加 SSE 重连后的补偿刷新与低频轮询。
- [ ] 发版后复测代理开关、UDP Direct、V2/WS 勾选后的展示一致性。

