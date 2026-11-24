# TUN 地址/路由被静默清掉，健康检查漏判（"进了 tun 出不去"）

> 日期: 2026-11-24
> 状态: 已修复并实机验证（Windows 实机删路由 → 检测 → 原地修复）

## 现象

- TUN 适配器/网卡仍然存在，`IsTUNRunning()` 仍为 true；
- 但主机侧 TUN 的 IP（如 `100.212.0.2/32`）或 `0.0.0.0/1`、`128.0.0.0/1`
  两条分流路由已经被清掉；
- 表现为：报文进了 tun 却出不去（netstack 没有对应的主机路由/源地址），
  或旁路网关短暂失效、部分流量绕过 TUN 直连，而控制台/健康循环仍显示 TUN 正常；
- 只有等下次进程重启重建 TUN 才恢复，因此很难归因。

## 为什么会被"悄悄"清掉

原健康循环（`tunHealthLoop`）只判断三件事：

1. 读循环是否退出（`TUNReadLoopDead`）；
2. 设备句柄是否丢失（`TUNDeviceName() == ""`）；
3. 接口名是否还存在（`tun.InterfaceExists(name)`）。

接口名存在就认为 TUN 健康，从不比对 IP 和路由。以下情况都能让"名字还在、
状态已坏"成立：

- **Windows / Wintun**：适配器被 PnP 重新枚举或驱动复位后，会出现同名的新
  适配器实例（新 LUID）。旧 Wintun 会话句柄仍指向旧实例，而新实例是干净的，
  没有我们配置过的 IP 和分流路由；按名字检查永远通过。
- **Windows 外部网络事件**：`netsh`/网络重置/其它 VPN/ICS 等可能清掉接口地址
  或路由，但适配器别名保留。
- **Linux**：`tun0` link 还在，NetworkManager、`ip addr/route flush`、其它 VPN
  可以单独删掉地址或 `0/1`、`128/1` 路由。
- **macOS**：utun 接口在，但另一条 VPN 抢走/删除了分流路由。

因此不是"接口消失"，而是"接口在、合同被破坏"，旧检查天然看不到。

## 修复

新增 TUN 运行时状态校验，并**并入现有 `tunHealthLoop`**（没有新增独立循环）：

- `TUNStateIssue`：`OK` / `InterfaceGone` / `IdentityChanged` /
  `AddrMissing` / `RouteMissing`；
- `RouteManager.VerifyRuntime()`：只读比对缓存的 TUN IP、`0/1`、`128/1`
  以及实际到 `1.1.1.1` 的最优路由；枚举失败按"未知/OK"处理，避免一次
  临时 API 错误触发破坏性修复；
- `RouteManager.Reassert()`：只补缺失的地址/路由/弱主机/exclusion，
  不重建设备；
- `tunHealthLoop`：接口存在时先校验，`AddrMissing`/`RouteMissing` 原地修复，
  修复失败或 `IdentityChanged` 才回退到 `StopTUN`/`StartTUN` 全量重建；
- 身份变化（LUID/index 变了）无法原地修：缓存的 Wintun 会话/旧实例已经失效，
  必须重建，这是唯一合理的回退路径。

平台实现：

- `tun/verify_windows.go`：iphlpapi（GetIpForwardTable2 / CreateIpForwardEntry2 /
  setInterfaceIPAPI / weak-host）；
- `tun/verify_linux.go`：netlink LinkByName / AddrList / RouteList / RouteReplace；
- `tun/verify_darwin.go`：InterfaceByName + route get 探测，地址已存在时跳过
  重复 `setIfaceAddr`（避免 EEXIST）。

## 验证

- Windows 本机、Linux/amd64、Darwin/arm64 交叉编译通过；
- `go vet` 仅剩既有 Windows 旧路由 API 的 unsafe.Pointer 告警，无新增；
- 实机（Windows，`phaethontun`，`100.212.0.2/32`）受控测试：
  先校验为 OK → 删除真实 `128.0.0.0/1` → 校验返回
  `split route 128.0.0.0/1 missing` → `Reassert` 原地恢复 → 再次校验 OK，
  全程未停止/重建 TUN 设备。

## 残余风险

- 若外部程序每 5s 反复清路由，健康循环会每个 tick 修一次并打 warn；
  这是持续的外部竞争，需要定位外部来源，但期间业务仍可用；
- 身份变化只能整机重建，重建瞬间 mesh 会话不断（netstack/P2P 不重启）。
