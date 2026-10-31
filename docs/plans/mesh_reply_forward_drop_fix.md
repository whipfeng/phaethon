# Mesh 转发流 reply 方向丢包修复设计

- 版本: v0.1.0
- 状态: DRAFT
- 日期: 2026-10-31

## 背景（已实锤的根因链）

QG 节点上 mesh 节点间转发流（VM↔GG TCP、VM↔jf/qg DNS UDP）间歇性单向丢包（"可以去不能回"），日志实锤根因：

1. 转发流首包进入 QG 时，conntrack 条目被置 `sourceManip=manipPerformed`（SNAT 曾执行——对 src=100.x mesh 地址执行了本只该对 LAN 客户端执行的 SNAT，精确触发条件见 §"毒源实锤"）。
2. 条目存活期间，reply 方向包在 conntrack.go:996-1000 被强制 `pkt.OutputNICName = cn.originalInputNIC`（记录的是无地址的 LinkNIC，如 link_vm）。
3. `forwardUnicastPacket` 走 OutputNICName 分支（ipv4.go:858-880）→ `FindRouteViaNIC(stack.go:1927)` → `getAddressEP` → `nic.primaryEndpoint` → LinkNIC 无地址端点 → `ErrNetworkUnreachable` → 丢包 + 回 ICMP unreachable。
4. 健康路径（`outputNIC=""`）走 `FindRoute` → RouteSelector 完全正常，**不需要地址端点**。
5. conntrack 条目超时（~50s/数分钟）后自愈，表现为"控制台刷新/等一会就恢复"。

关键事实：phaethon mesh 代码只为 NIC 1（tun）绑地址（netstack.go:698 GIP、netstack.go:833 VIP），**LinkNIC 从不绑地址**（netstack.go:335-337 创建后无 AddProtocolAddress）。`FindRouteViaNIC` 的"出口 NIC 必须有 primary 地址端点"检查是为本机产生流量设计的，对自带 src 的转发包是错误前提——但 patch #6 的机制本身不能删（见下）。

## 目标

1. mesh 转发流 reply 方向不再丢包（用户可见症状消除）。
2. LAN 客户端（192.168.1.x 经 tun）reply 路径不受影响。
3. 实锤 SNAT 误命中 mesh 流的条件，修掉毒源（或给出明确的不修复理由）。

## 非目标

- **不给 LinkNIC 绑地址**：用 hack 迎合错误检查，还会改变 RouteSelector/地址解析行为。
- **不删除 patch #6 的 OutputNICName 机制**：LAN 客户端 reply（dst=192.168.x）在路由表中没有条目，必须经 OutputNICName → FindRouteViaNIC(tun) 出口（tun 有 GIP/VIP 地址端点，该路径成功）。该机制对 LAN 是刚需。

## 设计决策

### D1（唯一改动）：毒源修复——performNAT 仅在真实 SNAT 时记录 originalInputNIC

修改点：`gvisor-fork/pkg/tcpip/stack/conntrack.go` performNAT（:824-827），条件加 `changeAddress`：

- noop SNAT（`maybePerformNoopNAT`，空地址端口占位）毒化的连接：`originalInputNIC` 保持空 → reply 的 `OutputNICName` 被置空 → 走健康 FindRoute（RouteSelector）路径。
- LAN 流（真实 SNAT，Input/Postrouting 命中 `inNic="tun"` 规则，`changeAddress=true`）：正常记录 `originalInputNIC="tun"` → patch #6 的 OutputNICName 机制原样工作。

修复后 `originalInputNIC` 只可能是 `"tun"`（SNAT 规则 `InputInterface="tun"` 精确匹配），`OutputNICName ∈ {"", "tun"}`，而 tun 有 GIP+VIP 地址端点，`FindRouteViaNIC(tun)` 必然成功——丢包路径在结构上被消除。

### D2（毒源实锤，2026-10-31 已定论）

**实锤结论：毒源不是 SNAT 规则误命中，而是 gVisor 原生 `maybePerformNoopNAT`。**

证据链：

1. `/tmp/phaethon-nat-action-debug.log`（41437 行）：真实 SNAT 命中（`targetAddr=100.0.0.1`）只有 37156 行，全部 srcAfter 改写为 VIP，且 srcBefore 只有 192.168.1.x / 100.0.0.2——**mesh 源从未命中 SNAT 规则**，H1/H2 排除。
2. 其余 ~4281 行 `targetAddr=`（空）且 srcBefore==srcAfter：全部来自 conntrack.go:769 `maybePerformNoopNAT` → `snatAction(..., tcpip.Address{}, changePort=true, changeAddress=false)`——gVisor 原生逻辑：凡过了 Input/Postrouting NAT hook 却未被规则 NAT 的连接，都做"空地址端口重映射 noop"并**把 `sourceManip` 标记为 `manipPerformed`**。
3. fork 补丁 conntrack.go:824-827 在 performNAT 里**无条件**记录 `cn.originalInputNIC = pkt.InputNICName`——noop SNAT 也记。于是每个 mesh 转发流（Postrouting hook，ipv4.go:601）和每个本地投递的 mesh 流（Input hook）都被毒化。
4. patch #6（conntrack.go:996-1000）见 `sourceManip=manipPerformed` 就把 reply 强绑 `OutputNICName=originalInputNIC`（无地址的 LinkNIC）→ 丢包。

间歇性来源：noop 只在连接首包实际遍历对应 hook 时触发一次（writePacketPostRouting / Input 路径），与路由类型、链路切换相关——有的流首包未遍历即不毒化，表现为时通时不通。

**修复**：conntrack.go:824-827 仅在 `changeAddress=true`（真实 SNAT 改写）时记录 originalInputNIC。noop SNAT 毒化的连接 originalInputNIC 保持空 → reply 的 OutputNICName 被置空 → 走健康 FindRoute 路径。LAN 流（真实 SNAT，Input hook 命中 inNic="tun" 规则）不受影响，patch #6 设计行为保留。

### D3（否决的方案，记录备查）

| 方案 | 否决理由 |
|---|---|
| 给 LinkNIC 绑地址端点 | 迎合错误检查；改变 RouteSelector 行为；掩盖毒源 |
| 删除 conntrack 的 OutputNICName 强绑 | LAN reply（192.168.x 无路由条目）依赖该机制出口，删了 LAN DNS 会破 |
| 修改 FindRouteViaNIC 取消地址检查 | 影响所有调用方（含本机流量），风险大于收益 |
| FindRouteViaNIC 失败 fallback 到 FindRoute（曾实现后撤回） | D1 修复后 OutputNICName 只可能是 tun，兜底分支是死代码；不给不可能发生的场景加防御 |

## 验收标准

1. `go build ./...`、`go vet ./tun`、`go test ./tun` 通过（gvisor-fork 变更须 `go build -a`）。
2. VM 部署后：QG 抓 poison 窗口内 `[forward] FindRouteViaNIC FAILED` 消失，出现 fallback 成功转发日志。
3. LAN 客户端 DNS/上网回归正常（patch #6 原场景不破）。
4. GG 链路 probe（loc.gg.phn / loc.jf.phn）长窗口无 ok=False 突发。
5. 毒源实锤结论写入本文档（H1/H2 定论 + 修复或遗留说明）。

## 部署

- VM first（本机），用户确认后 QG。gvisor-fork 变更必须 `go build -a` 强制重建。
- VM 一条命令 stop+replace+start；QG `rc-service phaethon restart`。
- 构建带版本戳（git describe），禁止猜版本号。
