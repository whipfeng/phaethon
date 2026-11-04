# TUN 残留清理作用域收敛与 darwin PF_ROUTE 结构化改造设计

## 任务信息

- 创建日期：2026-11-04
- 状态：DRAFT → 实现中
- 来源：优雅关闭挂死修复（见 [graceful_shutdown_and_tun_cleanup_design.md](graceful_shutdown_and_tun_cleanup_design.md)）实测时暴露的两个次生缺陷 + darwin 路由操作方式改进

## 背景

### 缺陷 1：darwin 无差别删除 split-tunnel 路由

`cleanup_darwin.go` 第一步无条件 `route delete -net 0.0.0.0/1 + 128.0.0.0/1`。0/1+128/1 是业界标准 split-tunnel 形态（WireGuard/OpenVPN/Clash-TUN 等均使用），同机运行其他 VPN 时 phaethon 的清理会把对方的路由删掉 → 对方流量泄漏走物理默认路由或直接断流。

### 缺陷 2：命中后 down 整个接口 + 198.18/15 兜底误伤面

- darwin `ifconfig <utun> down`、linux `ip link set down` 把整个接口打掉。本次 Mac 实测中 utun6 即因此被 down（后手动恢复）。
- 兜底匹配段 198.18/15：Clash/Mihomo fake-ip 默认使用 198.18.0.0/16，部分企业 VPN 也用该段——同机其他 VPN 的 utun/tun 持有 198.18.x.x 地址时会被误删地址 + down 接口，对方 VPN 直接死亡。
- 198.18/15 是 pre-mesh 老版本 fakeip 的遗留。按项目规则（不留旧版本兼容）应删除；tunIP 现在每条启动路径都有值（`residualTUNIP()` / `e.meshHostIP`）。

### 改进动机：darwin 是路由操作唯一的"文本解析"短板

| 平台 | 现状 | 对象化程度 |
|------|------|-----------|
| linux | netlink | 已结构化（link handle / index 精确操作） |
| windows | iphlpapi（LUID/Index） | 已结构化 |
| darwin | `route`/`netstat` shell 文本命令 | 短板，无文本解析就无法精确按接口操作 |

darwin 的正解是 **PF_ROUTE socket**（地位等同 linux netlink）：`RTM_ADD/RTM_DELETE/RTM_GET` 消息携带 `rtm_index`（接口 index）与变长 sockaddr 列表，按接口精确加删路由、读取默认网关，全程结构化。

## 目标

1. 残留清理只作用于"持有本节点 tunIP 的接口"，对任何非 phaethon 接口/路由零触碰。
2. 删除 198.18/15 兜底匹配。
3. darwin 路由操作从 shell 文本命令迁移到 PF_ROUTE 结构化消息，三平台操作方式对齐。

## 关键设计决策

### 决策 1（阶段一）：清理行为收敛，darwin/linux

**darwin `CleanupResidual`：**
- 删除开头的全局 `route delete 0/1+128/1`（缺陷 1 根因）。
- 只扫 utun 接口、只按 `ip4.Equal(tunIP)` 精确匹配；命中后仅 `ifconfig <name> delete <ip>`。
- 不 down 接口。依据：darwin 上挂接到 utun 地址的 split-tunnel 路由随地址删除被内核自动移除（前序 Mac 实测已验证）；无地址的 utun 无害，下次 Setup 照常复用。
- 未匹配 → 不动任何路由（0/1 路由若存在，归属者不是 phaethon）。
- `tunIP == nil` → 跳过接口扫描（windows 测试入口 `_recreate_check` 传 nil，仅影响 windows，签名行为对齐）。

**linux `CleanupResidual`：**
- 先用 netlink 扫描 `tun*` 接口地址，**仅当**某接口持有 tunIP 时：
  - 按该 link index 删 0.0.0.0/1 + 128.0.0.0/1 路由（netlink `RouteDel`，已结构化）；
  - `netlink.AddrDel` 删地址（替换 `ip addr del` 外部命令）。
- 不 `ip link set down`。
- resolv.conf 恢复 / tunState 恢复（rp_filter、ip_forward、iptables FORWARD 清理）保持无条件执行——它们是 phaethon 自有文件与规则的兜底恢复，与接口匹配无关，`tunIP == nil` 也执行。

**windows**：不变（按适配器名 phaethontun 对象化清理）。

### 决策 2（阶段二）：darwin 路由操作迁移 PF_ROUTE

新增 `tun/routesock_darwin.go`：PF_ROUTE socket 封装（纯 syscall，不引第三方库）：

- `routeAddInterface(index int, dst *net.IPNet)`：`RTM_ADD`，addrs=RTA_DST|RTA_NETMASK|RTA_GATEWAY，gateway=sockaddr_dl（AF_LINK, sdl_index=接口）——替代 `route add -net x -interface utunN`。
- `routeAddGateway(dst *net.IPNet, gw net.IP)`：`RTM_ADD`，addrs=RTA_DST|RTA_NETMASK|RTA_GATEWAY，gateway=sockaddr_in，flags 带 RTF_GATEWAY——替代 exclusion 路由的 `route add -net/-host x <gw>`。
- `routeDelete(index int, dst *net.IPNet)`：`RTM_DELETE`，带 rtm_index 精确限定接口——替代 platformTeardown 的全局 delete。
- `defaultRoute() (gw net.IP, ifName string, err error)`：`RTM_GET` 查询 0.0.0.0/0，从响应解析 RTA_GATEWAY（sockaddr_in）与 RTA_IFP（sockaddr_dl 的 sdl_data 接口名）——替代 `netstat -rn` 文本解析。

`route_darwin.go` 改造点：`platformSetup`（split-tunnel add、exclusion add）、`platformTeardown`、`deleteExclusionRoute`、`getDefaultGatewayDarwin` 全部改走 PF_ROUTE。地址配置保留 `ifconfig`（设置接口地址无更合适的公开 API；读取已由 Go stdlib 的 getifaddrs 封装）。

### 决策 3：198.18/15 兜底删除

darwin/linux 的匹配条件从「198.18/15 或 tunIP」收敛为「仅 tunIP 精确相等」。项目未发布、无 198.18 历史部署，兜底只带来误伤面无收益。

## 不做的事

- 不改 windows 清理与路由路径。
- 不改优雅关闭流程、watchdog、引擎结构。
- darwin 地址设置不迁 ioctl（保留 ifconfig），只迁路由与网关探测。

## 风险

| 风险 | 缓解 |
|------|------|
| PF_ROUTE 消息 sockaddr 变长/对齐易错 | 只实现本项目需要的 IPv4 形态（sockaddr_in 16B、sockaddr_dl 20B 定长），写后读回校验 rtm_errno；Mac 实测加删路由验证 |
| 删全局 route delete 后，极端残留形态（接口在、地址在、路由是外部工具后加的）清不掉 | 该形态路由挂在本节点 tunIP 地址所在接口上，删地址时内核一并移除；如未来出现反例再按接口 scope 补删 |
| linux 未匹配时不再删 tun0 的 0/1 路由 | tun0 存在即有其 fd 持有者（活着），清理本就不该动；phaethon 自己的 tun0 残留必然带 tunIP 地址，会先匹配上 |

## 验收标准

1. `GOOS=windows/linux/darwin go build ./tun/...` 全通过。
2. Mac 实测：
   - kill -9 残留 utun 被下次启动清理（cleanup 日志 + ifconfig 复核）；
   - 手工在另一 utun 注入 198.18.x.x 地址 + 0/1 路由 → 触发清理 → 该接口与路由完好无损；
   - TUN 开启/关闭正常，路由加删正确（`netstat -rn` 复核）。
3. `tests/e2e/tun/_recreate_check`（windows）通过。
