# darwin 全篇审计 + 加固 设计

> 背景：split-route-conflict 修复过程中发现 xnu RTM_GET 回包 netmask 截断编码
> 导致 preempt 静默失效（已修）。按"类似问题全篇审视"指令对全部 darwin 文件
> 做同类审计（xnu 编码陷阱 / 文本解析脆弱性 / 部分失败状态泄漏），
> 并承接遗留任务项：ifconfig×3 exec → 结构化 API 替换。

## 审计清单（全 darwin 文件逐一过）

| 项 | 位置 | 判定 |
|---|---|---|
| A | routeProbeSplit NETMASK 截断编码 | ✅ 已修（sockaddrNetmask 双形态） |
| B | platformSetup step 3 部分失败（0/1 加完 128/1 失败） | **修**：defer 只恢复 foreign，不删自己已加 split → foreign 恢复 EEXIST 被忽略 → 异主 0/1 永久丢失 + 自家 0/1 残留且 applied=false 无 Teardown 机会 |
| C | restoreForeignSplits 只校验接口 index 存在 | **修**：utun index 会被复用，preempt 后 holder 死亡、别的进程抢同 index → 恢复到错误接口。快照补接口名，restore 校验名字一致 |
| D | getDNSServers 依赖英文串 "There aren't any DNS Servers" 判 DHCP | **修**：networksetup 输出随系统语言本地化，中文系统下判不中 → 中文字符串当 DNS 服务器行被保存并在 restore 时写回系统。行级 net.ParseIP 校验，零可解析行 = DHCP |
| E | ifconfig×3 exec（route_darwin setup/down + cleanup delete） | **修**：SIOCAIFADDR / SIOCSIFFLAGS / SIOCDIFADDR（遗留任务项） |
| F | defaultRoute GATEWAY 全形 sockaddr | 无需改：探针实证 RTM_GET 的 DST/GATEWAY 恒为 16 字节全形，xnu 仅截断 netmask |
| G | dialer/routesock 只读 rtm_index 不解析 sockaddr | 无需改 |
| H | parseSockaddrs /0 netmask sin_len=0 break | 无需改：defaultRoute 不消费 netmask；probeSplit 遇 /0 得 nil → 非 /1 正确结论 |
| I | preempt 探测失败 → 后续 add EEXIST 报错退出 | 现状合理：defer 恢复快照为空，行为与修复前一致（enable 报错由上层重试） |
| J | device_darwin Write 每次 append 分配 | 非正确性问题，不动 |
| K | cleanup 只按 tunIP 精确匹配接口 | 无需改：holder 198.18 天然免疫 |
| L | findActiveNetworkService 首匹配即返回 | **修**（验证 D 时实证）：① 解析分支顺序错误——"(Hardware Port:…)" 行被服务名分支截胡，函数恒返回 error，DNS 重定向从未生效；② 即使顺序正确，PPPoE 与 Wi-Fi 同挂 en0 会把 DNS 写到未连接服务。改为分支顺序修正 + 候选按 getinfo 含 IPv4 筛选 |
| M | manager.go setSystemDNS 传 dev.Name()（utun7） | **修**（验证 L 时实证）：候选过滤器排除 utun/tun 设备 → 恒无匹配 → DNS 重定向静默失败。改传 DefaultIfaceName（物理口 en0） |

## 修复设计

### B：platformSetup 失败回滚（tun/route_darwin.go）

defer 改为：先无条件 `routeDeleteNetViaInterface` 0/1+128/1（ESRCH 不算错），
再 `restoreForeignSplits()`。正常成功路径 defer 不执行；任何失败路径下：
- 自己残留的 split 被清掉（foreign 恢复不再有 EEXIST 遮挡）
- foreign 快照按原形态恢复

### C：快照接口名校验（tun/route.go + route_darwin.go）

```go
type splitRouteSnapshot struct {
    dst     *net.IPNet
    gwIP    net.IP
    gwIndex int
    gwName  string // preempt 时记录的接口名，restore 时校验防 index 复用错接
}
```

preempt 时 `net.InterfaceByIndex(snap.gwIndex)` 取名（探测回包的 index 必然存活）。
restore 时接口存在但名字不符 → info 日志跳过。

### D：getDNSServers 行级 IP 校验（tun/dns_system_darwin.go）

每行 `net.ParseIP` 校验，收集可解析行；零行可解析 → `(nil, true, nil)` 视为 DHCP。
不再依赖任何英文/本地化文本判据（"not set" 等串保留为 DHCP 的快速路径，但不再是唯一判据）。

### L：findActiveNetworkService 活跃性筛选（tun/dns_system_darwin.go）

同接口可能挂多个服务（PPPoE + Wi-Fi 同挂 en0），首匹配会把 DNS 写到未连接
的服务。改为：枚举 device 匹配的候选 → 选 `networksetup -getinfo` 输出中
含可解析 IPv4 的首个服务；无活跃者退回首个候选（旧行为）；候选为空报错。

### E：ifconfig×3 → ioctl（tun/route_darwin.go + tun/cleanup_darwin.go）

**setup 配地址**（route_darwin.go platformSetup step 0）：
`ifconfig utunN inet A A netmask M up` =
1. `SIOCAIFADDR`：unix.IfAliasreq{Name, Addr: SockaddrInet4{A}, Dstaddr: SockaddrInet4{A}, Mask: SockaddrInet4{M}}
   （utun 是 POINTOPOINT，dst=addr 复刻 ifconfig 行为）
2. `SIOCGIFFLAGS` → `|= IFF_UP` → `SIOCSIFFLAGS`
socket：unix.Socket(AF_INET, SOCK_DGRAM, 0)。返回错误带 errno。

**teardown down**（platformTeardown）：`SIOCGIFFLAGS` → `&^= IFF_UP` → `SIOCSIFFLAGS`。

**cleanup 删地址**（cleanup_darwin.go CleanupResidual）：`ifconfig utunN delete ip`
→ `SIOCDIFADDR`：unix.Ifreq{Name, Addr: SockaddrInet4{ip}}。SIOCDIFADDR 内核按
addr 匹配 ifa（无 mask 参数），语义与 ifconfig delete 完全一致；iface.Addrs() 已有
的 mask 仅用于展示不再传内核。

## 验证计划（Mac 真机）

1. 三平台 go build
2. phase_a（开关）/ phase_b（kill -9）/ phase_d（holder 共存+恢复）全过
3. 专项：SIOCAIFADDR 后 `ifconfig utunN` 输出与 ifconfig 版逐字节一致（inet A --> A netmask 0xfffffff8）
4. 专项：mask 匹配语义——enable/disable 后 CleanupResidual 走 SIOCDIFADDR，phase_d holder 全程完好复核
5. DNS：enable 后 Wi-Fi 服务 DNS = TUN DNS 地址（实测此前被 PPPoE 遮蔽从未生效），disable 后恢复 100.0.0.3
6. 运行时日志零错误、CPU 0%
