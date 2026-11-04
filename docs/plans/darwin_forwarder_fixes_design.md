# darwin 转发修复设计：控制台两个固定错误 + readLoop 忙轮询 + exec 收敛

> 背景：Mac 真机验证时发现控制台连接日志两个固定错误（`no route to host` / `listen udp :0: invalid argument`）+ TUN 空闲时 readLoop 100% CPU。探针实证定位，全部限定 darwin 文件改动，windows/linux 零触碰。

## 实证结论（bindprobe 探针，TUN 开启持有 0/1+128/1 时）

| 绑定方式 | 无 TUN | 有 TUN（现状） | 加 ifscope 物理默认后 |
|---|---|---|---|
| LocalAddr+IP_BOUND_IF(en0) | OK | **no route to host** | OK |
| IP_BOUND_IF(en0) only | OK | **network is unreachable** | OK |
| LocalAddr only | OK | **no route to host** | OK |
| UDP listen :0 + IP_BOUND_IF | — | **invalid argument** | （不变，家族 bug） |

- **错误 1 根因**：xnu 的接口域（IP_BOUND_IF）查表在存在 0/1+128/1（PRCLONING 接口路由）时无法落到物理默认路由 → ENETUNREACH/EHOSTUNREACH。修复：platformSetup 加 0/1+128/1 的同时，追加 `default ifscope <物理接口> via <原网关>`（RTF_IFSCOPE=0x1000000 + rtm_index=物理口），物理口的 scoped 查表恢复可达；platformTeardown 删除。实测 route(8) 加同参数后全部 TCP 变体恢复 OK。IFSCOPE 路由对非 scoped 查表不可见，不影响普通流量。
- **错误 2 根因**：Go `ListenPacket("udp", ":0")` 在 darwin 创建 **IPv6 双栈 socket**；setBoundIf 按 dst 家族选 sockopt，对 v4 dst 设 IPPROTO_IP/IP_BOUND_IF → IPv6 socket 上 EINVAL。修复：setBoundIf 改为 **按 socket 实际家族**（Getsockname 判定）设 IP_BOUND_IF 或 IPV6_BOUND_IF。
- **100% CPU 根因**：device_darwin.go 非阻塞 fd + readLoop EAGAIN 直接 continue → 空闲忙轮询。修复：darwinTUN.Read 内 poll(POLLIN, 100ms) 后再读，EAGAIN 路径实际上不再触发；close 后经 EBADF/poll 错误路径退出，tunCloseCh 检查兜底。

## 改动清单（全部 darwin 限定）

1. **tun/routesock_darwin.go**：新增 `routeAddIfscopeDefault(gw, ifIndex)` / `routeDeleteIfscopeDefault(gw, ifIndex)`（RTM_ADD/DELETE，flags UP|GATEWAY|STATIC|IFSCOPE，rtm_index=物理口，**必须带 RTA_NETMASK(0.0.0.0)**——真机对照实证：缺 netmask 时内核虽接收路由且 netstat 显示 UGScIg，但 scoped 查表与 route(8) delete 均报 "not in table"，等同没加；EEXIST 视为已应用，teardown 时删除）；新增 `routeGetIfIndex(dst) (int, error)`（RTM_GET 返回 rtm_index，替代 `route -n get` 解析）。
2. **tun/route_darwin.go**：platformSetup 在 0/1+128/1 之后加 ifscope 物理默认（需 originalGateway 已知）；platformTeardown 逆序删除。记录 applied 标志。
3. **dialer/routesock_darwin.go**（新文件）：dialer 包自含的精简 PF_ROUTE 实现（仅 RTM_GET，~120 行，不依赖 tun 包避免循环引用）。
4. **dialer/bind_darwin.go**：darwinRouteIfaceIndex 改走 PF_ROUTE routeGetIfIndex（删 exec.Command("route")）；setBoundIf 按 socket 家族设 sockopt。
5. **tun/device_darwin.go**：Read 内嵌 poll 等待可读，消除 EAGAIN 忙轮询。

## exec 用法全量研判

| 位置 | 命令 | 路径热度 | 可否 API 化 | 结论 |
|---|---|---|---|---|
| dialer/bind_darwin.go:76 | route -n get | **每连接每 dst** | PF_ROUTE RTM_GET | **本次替换** |
| tun/route_darwin.go:17 | ifconfig utun inet … up | TUN 开关一次 | SIOCAIFADDR ioctl | 可行，另立任务（需专项验证） |
| tun/route_darwin.go:89 | ifconfig utun down | TUN 关一次 | SIOCSIFFLAGS ioctl | 可行，另立任务 |
| tun/cleanup_darwin.go:47 | ifconfig utun delete ip | 残留清理 | RTM_DELADDR | 可行但 mask 匹配语义需专项测试（保 198.18 共存行为），另立任务 |
| tun/dns_system_darwin.go ×4 | networksetup | TUN 开关 | 无（仅 SCNetworkConfiguration cgo） | 保留 |
| tun/dns_system_linux.go ×4 | resolvectl/nmcli | TUN 开关 | 无纯 Go 等价 | 保留 |
| tun/route_linux.go ×5 / cleanup_linux.go ×3 | iptables | TUN 开关 | netlink 可替但复杂度高收益低 | 保留 |
| util/browser.go ×3 | rundll32/open/xdg-open | 用户点击 | 无 | 保留 |
| cmd/ifdiag、pkg/signing | netsh/route/ping/strings | 诊断/签名工具 | 非产品路径 | 保留 |

## 验证

- 三平台 `go build`（windows/linux 零改动但必须过编译）
- Mac 真机：TUN 开启后 bindprobe 全矩阵（复用 tests/e2e/tun/mac_verify/bindprobe）
- Mac 真机：控制台连接日志不再出现两类错误；curl 外网经 TUN 转发成功
- Mac 真机：enable 后进程 CPU 不再 100%（`top -pid` 采样）
- Mac 真机：disable/teardown 后 ifscope 默认路由删除（netstat 核对）
- 回归：198.18 共存（phase_d）、kill -9 回收（phase_b）、TUN 开关（phase_a）
