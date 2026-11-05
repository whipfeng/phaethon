# DNS 经系统路径解析失败 — Windows 源地址约束路由致查询未进 TUN

**日期**: 2026-11-05
**状态**: 已修复并验证（2026-11-05 16:53 本 VM 部署，系统路径解析全通；方案见 [windows-dns-tun-registration-plan.md](../plans/windows-dns-tun-registration-plan.md)）
**影响面**: Windows 节点（源地址绑定到物理网卡地址的查询一律到不了 hijacker）

## 症状

- phaethon 运行正常、TUN 正常、数据面正常（curl 直连 200、nslookup 直连 hijacker 秒回）
- 但系统路径（dnscache 服务）解析全部失败，**fakeIP 域名（www.baidu.com）和 .phn 域名同样失败**
- 表现为"DNS 间歇性挂掉"，用户/agent 切公网 DNS 急救后循环复发

## 现场证据链

### 第一轮（15:33–15:45）— 二分定位到源地址

1. **watcher v2 四路探针**（dnsSrv/sys/hijack/plane）：
   - `dnsSrv=100.212.0.3 sys=FAIL hijack=OK:100.0.0.33 plane=OK`
   - hijacker 健康、数据面健康、配置正确，唯独系统路径 FAIL —— 故障被锁定在"系统→hijacker"这一段
2. **手工二分探针**（源地址绑定二分）：
   - 不绑定源地址 → **OK**（OS 选 TUN 侧地址 100.212.0.2 作源）
   - 绑定 100.212.0.2（TUN 侧）→ **OK**
   - 绑定 172.30.0.2（以太网物理地址）→ **TIMEOUT**
   - 源地址决定生死。

### 第二轮（16:34，固定源端口 + 全链路 trace）— 实锤丢包点

探针改用固定端口（A=unbound / B=172.30.0.2:60002 / C=100.212.0.2:60003），gvisor debug 全程记录：

1. **B（物理源）的查询从未进入 netstack**：1MB trace 窗口内 `60002` 与 `src=172.30.0.2` **零命中**；TUN NIC 仅有两个 UDP/53 入站事件（A、C 各一）。
2. **网卡计数器实锤出口**：20 连发物理源绑定包，wintun `SentUnicastPackets` 增量 = 1（环境底噪）—— 一个都没进 TUN；全部被 OS 从物理网卡发出（送给网关 172.30.0.1，无回程）。
3. **SNAT/DNAT 设计链路工作完全正常**（针对到达的查询）：Input 链 SNAT 将全部查询源改 VIP（hijacker 看到 remote=100.212.0.1），hijacker 回包经 Output 链 conntrack DNAT 还原到原源地址，经 `pkt.OutputNICName=tun` 旁路从 NIC 1 发出（A/C 两路回包 trace 完整，WritePacket OK）。
   - 例：A/C 回包日志 `WritePacket CALL: src=100.212.0.3 dst=100.212.0.2 proto=17 nicID=1 nicName=tun OK` —— dst 还原为 100.212.0.2（原查询源），与本机 socket 完全对应，客户端秒收。

## 根因

**不是** netstack 空路由表丢回包（第一版误判，已纠正）。真实链路是：

```
dnscache 查 100.212.0.3（注册在物理网卡）
→ Windows 按"DNS server 所在接口"选源地址 = 172.30.0.2（物理网卡所有）
→ 显式绑定源的 socket 触发 Windows 源地址约束路由：
   出口接口被约束到"拥有该源地址的接口"（以太网），无视 /1 前缀优先生效
→ 包从物理网卡发给默认网关 172.30.0.1 → 100.212.0.3 不可达 → 死
→ 查询根本到不了 TUN / netstack / hijacker
→ dnscache 超时 → 用户感知"DNS 挂了"
→ （Mac agent）切公网 DNS 急救 → mesh .phn 域名死（副作用）
→ 重启 phaethon → 循环
```

而用户态直连工具（nslookup / Resolve-DnsName -Server / curl）**不绑定源地址**：
OS 按目的路由选出口（/1 前缀 → wintun），源地址自动选 wintun 侧 100.212.0.2，
源/出口一致 → 进 TUN → SNAT → hijacker → DNAT 还原 → 回包正常。**这解释了全部"时好时坏"表象**：
谁绑定物理源谁死，不绑定的都活；dnscache 的绑定行为随 Windows 版本/状态有差异。

**SNAT/DNAT 机制本身经实锤验证无缺陷**——conntrack 在 Input 链记录 `originalInputNIC=tun`，
回包 DNAT 正确还原 `cn.original.tupleID.srcAddr` 并强制从 tun 出口（gvisor-fork patch #6/#11）。
此前 trace 中 `dst=100.212.0.2` 是 A/C 两路 TUN 源查询的正确还原值，不是异常。

13:03 的 stall dump（138 goroutine 全部健康）与本次故障无因果——那是另一个独立问题（OS→wintun 交付层冻结），见 mesh-data-plane-stall.md。L1/L2 goroutine 泄漏同样不是本次根因。

## 引入点（git 实锤）

回归由 **6fa266e**（2026-11-04，"fix(darwin): 全篇审计加固"）引入。该 commit 修的是 macOS 真 bug（候选过滤器排除 utun 设备，传 TUN 设备名导致 DNS 重定向在 darwin 从未生效），但改动落在**跨平台**的 `tun/manager.go`：

```
- setSystemDNS(dev.Name(), dnsIP.String())      // DNS 注册在 TUN 适配器
+ setSystemDNS(e.routeMgr.DefaultIfaceName, ...) // DNS 注册在物理默认网卡
```

macOS 上这是修复；**Windows 上是回归**——6122504（2026-08-26）当初把 DNS 从物理网卡挪到 TUN 适配器的理由原文即 "so queries actually reach the DNS hijacker"（配物理网卡时查询到不了 hijacker），此后在 TUN 适配器上稳定运行 2.5 个月。6fa266e 在 darwin 上的验证（"enable=100.218.0.3 全通"）覆盖不到 Windows 路径。

Windows 上配物理网卡之所以"查询到不了 hijacker"，机理即本文根因：dnscache 绑物理源 → 源约束路由 → 物理出口。

## 修复方案（待批）

**主方向：DNS 注册接口平台分化**（最小改动，回到已验证 2.5 个月的形态）：

- Windows：`setSystemDNS(dev.Name(), ...)` 恢复注册到 TUN 适配器 → dnscache 以 wintun 侧地址为源 → 源/出口一致 → 查询进 TUN → SNAT/DNAT 链路（已实锤健康）→ 通的。同时恢复配套：TUN 适配器 metric 调低（6122504 用 metric=5）保证 Windows 优先选用。
- Darwin：维持 6fa266e 的 DefaultIfaceName（macOS 上传 TUN 设备名确实不匹配候选）。
- 实现形式：`tun/manager.go` 调用点按平台分流（build tag 或 routeMgr 提供平台化的 DNS 目标接口名），不碰 gVisor fork，不触碰 §1.2 空表设计。

**纵深认知（非行动项）**：本故障 gVisor 层无任何丢包——查询根本没到达。fork 加固（hijacker 回包 NIC 旁路）对本案无意义，撤销该建议。若未来确有"任意源地址查询都要通"的需求，正确下手点是 OS 路由层（host route / weak-host 发送约束），而非 netstack。

**验证门禁**（`data/dns-bind-probe-v2.ps1`，固定端口版）：

- A（unbound）/ C（绑 100.212.0.2）→ OK（修复前已 OK，保持回归）
- B（绑 172.30.0.2:60002）→ **修复后仍 TIMEOUT 属预期**：这是 Windows 源约束路由的固有行为（OS 把包发给物理网关），与 phaethon 无关，不作为门禁
- dnscache 系统路径 `Resolve-DnsName mb.qg.phn` + `www.baidu.com` → 全通（核心门禁）
- watcher v2 四路探针 `sys=OK`

## 诊断工具沉淀

- `data/stall-watch.ps1` v2：四路探针（dnsSrv/sys/hijack/plane）+ 5s 粒度路由/适配器/RX 采样。注意文件须 UTF-8 **带 BOM**（PowerShell 5.1 对无 BOM 文件按 GBK 解析）
- `data/dns-bind-probe-v2.ps1`：手工构造 DNS 查询 + **固定源端口** + 源地址绑定二分探针，可直接复用于验证修复
- 验证"OS 路由出口"技巧：`Get-NetAdapterStatistics` wintun `SentUnicastPackets` 增量（发包前/后采样），比 RX 噪声判别可靠
