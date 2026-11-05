# Windows 系统 DNS 注册回到 TUN 适配器（平台分流）

| | |
|---|---|
| 版本 | v1.0.0 |
| 状态 | 已实施并验证（2026-11-05 16:53，本 VM） |
| 关联 | [dns-source-constrained-routing.md](../issues/dns-source-constrained-routing.md)（根因实锤）、6fa266e（回归 commit）、6122504（历史已验证形态） |

## 一、背景与根因（一句话版）

6fa266e 把跨平台的 `setSystemDNS` 调用参数从 TUN 设备名改成物理默认网卡（为修 darwin）。Windows 上这触发源地址约束路由：dnscache 按"DNS server 注册接口"绑物理源地址发包 → 出口被约束到物理网卡 → 查询永远到不了 TUN 内的 DNSHijacker（20 连发实测 wintun 增量=0）。详见 issue 文档第二轮证据链。

## 二、目标与非目标

**目标**：
- Windows：系统 DNS 重新注册到 TUN 适配器（恢复 6122504 形态，已稳定运行 2.5 个月），dnscache 源/出口一致，查询进 TUN → SNAT/DNAT 链路（已实锤健康）→ 通。
- Darwin：维持 6fa266e 行为（传 `DefaultIfaceName`），不回退 macOS 修复。
- Linux/其他：行为不变（传 `DefaultIfaceName`）。

**非目标**：
- 不碰 gVisor fork、不碰 §1.2 空路由表设计（本故障 netstack 无任何丢包）。
- 不改 `setSystemDNS` 各平台实现，只改调用点的接口名选择。
- 不做"任意源地址查询都通"的 OS 路由层加固（issue 文档已撤销该建议）。

## 三、方案

调用点一处改动 + 每平台一个三行 helper：

1. `tun/manager.go:605`：`setSystemDNS(e.routeMgr.DefaultIfaceName, ...)` 改为 `setSystemDNS(dnsRegistrationIface(dev.Name(), e.routeMgr.DefaultIfaceName), ...)`，同步更新注释（现有注释描述的只是 darwin 理由，需注明平台分化）。
2. 各 `dns_system_*.go` 增加：

```go
// dnsRegistrationIface 选择系统 DNS 注册的接口名。
func dnsRegistrationIface(tunName, physicalName string) string
```

| 平台 | 返回值 | 理由 |
|---|---|---|
| windows (`dns_system_windows.go`) | `tunName` | Windows 源约束路由：注册到物理网卡 → dnscache 绑物理源 → 永远进不了 TUN。注册到 TUN 适配器后 metric=5（实现内已有）保证优先选用。 |
| darwin (`dns_system_darwin.go`) | `physicalName` | `findActiveNetworkService` 候选刻意排除 utun/tun 设备（6fa266e 修复点），传 TUN 名静默 no-op。 |
| linux (`dns_system_linux.go`) | `physicalName` | 行为不变（现状无故障报告；resolvectl/nmcli 按物理接口注册后查询经 TUN 分流进出，Linux 无 Windows 式源约束）。 |
| fallback (`dns_system_fallback.go`) | `physicalName` | no-op，无影响。 |

`restoreSystemDNS(e.device.Name())`（manager.go:683）各平台行为已正确（darwin 走内部保存的 service，windows 本来就收 TUN 名），**不动**。

## 四、风险

| 风险 | 评估 |
|---|---|
| darwin 回退 | 无——darwin 返回值与 6fa266e 后现状完全一致，零行为变化 |
| Linux 变化 | 无——返回值不变 |
| Windows 回归 | 低——恢复的是 6122504 后稳定 2.5 个月的形态；且 `setSystemDNS` windows 实现（metric=5/flush cache）本身就是按 TUN 名设计，现传物理名反而是错配 |
| 双 agent 同机 | 实施前看 qoder pane；部署窗口避开对方操作 |

## 五、验证门禁

1. `go build ./...` 全平台交叉编译（windows/darwin/linux）通过。
2. 本 VM 部署后：
   - `data/dns-bind-probe-v2.ps1`：A/C 用例 OK；B 用例仍 TIMEOUT（Windows 源约束路由固有行为，预期）。
   - `Resolve-DnsName mb.qg.phn` + `www.baidu.com` 系统路径全通（核心门禁）。
   - watcher v2 四路探针 `sys=OK`。
   - `Get-DnsClientServerAddress -InterfaceAlias <TUN>` 显示 100.212.0.3，metric=5。
3. 持续观察 ≥30 分钟无"DNS 间歇性挂掉"复发。
