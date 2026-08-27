# 统一跨平台 TUN 实现 + 清理机制

## 元数据

- 文档类型：Plan
- 版本：v0.2.0
- 所属项目：phaethon
- 创建日期：2026-08-25
- 变更记录：
  - v0.2.0（2026-08-26）：移除“引擎级健康看门狗”目标，改为“统一清理机制 + 外部 watchdog 兜底”；明确 Linux/macOS DNS 与清理逻辑。

## 背景

TUN 模块当前仅在 Windows 上可用（`tun/avail_other.go` 硬编码返回 `false`），但 Linux/macOS 的设备创建文件已存在。macOS 路由设置缺少接口 IP 与 `ifconfig up`；DNS 目前只在 netstack 内部劫持，没有把系统 DNS 重定向到 TUN；且一旦 TUN 把路由/DNS 改乱，可能把当前工作机网络搞挂。

Windows 已有进程级看门狗（`LAYER_WATCHDOG_PID` 监控父进程退出后清理），但缺少引擎级健康看门狗。经评估，**进程内看门狗无法可靠探测 TUN DNS 路径**（本进程发出的探测包不会经过 wintun 回环到同进程），容易误触发。因此本次把“引擎级看门狗”改为“统一清理机制 + 外部 watchdog 兜底”，保证崩溃/异常退出后能恢复网络。

## 目标

1. 跨平台启用 TUN（Linux/macOS 不再默认不可用）。
2. 补齐 macOS 接口 IP 与路由。
3. 补齐系统 DNS 重定向（Windows/Linux/macOS）。
4. 统一各平台清理逻辑，确保崩溃后网络可恢复。
5. 不在当前工作主机启用真实 TUN 整机测试。

## 非目标

- 不做引擎级内部健康看门狗。
- 不改 TUN 核心转发架构（netstack + split-tunnel 0/1 + 128/1）。
- 不做 IPv6 优先/完整 IPv6 DNS 劫持。
- 不改动 Windows 现有 wintun 适配器创建流程。

## 阶段

### 阶段 1：跨平台可用性开关

修改 `tun/avail_other.go`：

- `Available()` 在 Linux/Darwin 上返回 `true`。
- 保留运行时最小检查：Linux 检查 `/dev/net/tun` 是否可打开；Darwin 检查 `com.apple.net.utun_control` 是否可连接。失败时记录 warn 并返回 `false`，而不是编译期硬编码。

### 阶段 2：macOS 接口与路由

在 `tun/route_darwin.go` 的 `platformSetup` 中，在检测默认网关之前：

1. 使用 `exec.Command("ifconfig", devName, "inet", tunIP, tunIP, "netmask", mask, "up")` 配置接口 IP。
2. 使用 `exec.Command("ifconfig", devName, "up")` 确保接口 up。
3. 保存 `DefaultIfaceName` / `DefaultIfaceIndex`，与 Linux 行为一致。

`platformTeardown` 中删除 split-tunnel 路由后，调用 `ifconfig devName down` 做最佳努力清理。

### 阶段 3：系统 DNS 重定向

新增 `tun/dns_system.go`（公共接口）并按平台 build tag 拆分实现：

- **Windows**（`tun/dns_system_windows.go`）：`netsh interface ip set dns name=devName static <tun-ip>`；停止时恢复为 DHCP 或保存的原 DNS。
- **Linux**（`tun/dns_system_linux.go`）：
  - 检测 `systemd-resolved`：使用 `resolvectl dns devName <tun-ip>`。
  - 检测 `NetworkManager`：使用 `nmcli` 设置接口 DNS。
  - 回退：备份 `/etc/resolv.conf`，写入 `nameserver <tun-ip>`；停止时恢复备份。
- **macOS**（`tun/dns_system_darwin.go`）：
  - `networksetup -listnetworkserviceorder` 找到活跃服务。
  - `networksetup -setdnsservers <service> <tun-ip>`；恢复时用 `Empty` 表示 DHCP。

`Engine.Start()` 在 `routeMgr.Setup` 成功后调用 `setSystemDNS(devName, tunIP)`；`Engine.Stop()` 在 route teardown 前调用 `restoreSystemDNS()`。

新增 `tun/dns_system_test.go`：用 mock exec 或只测 backup/restore 辅助函数，不真正改系统 DNS。

### 阶段 4：统一清理逻辑

- 新建 `tun/cleanup_other.go`：
  - Linux：删除 `0.0.0.0/1`、`128.0.0.0/1` 路由；尝试 `ip link set devName down`。
  - Darwin：删除 `0.0.0.0/1`、`128.0.0.0/1` 路由；`ifconfig devName down`。
- 更新 `tun/cleanup_windows.go`：
  - 已较完整，增加删除系统 DNS 静态配置（恢复 DHCP）作为兜底。
- 所有清理函数只记录 warn，不返回错误，确保即使部分失败也继续执行。

### 阶段 5：外部 watchdog 兜底

- 保留 Windows 现有的 `LAYER_WATCHDOG_PID` 进程看门狗：父进程退出后自动清理路由/DNS。
- 在 `CleanupResidual` 中增加 DNS 恢复兜底，确保下次启动或看门狗触发时能恢复系统 DNS。
- 不在进程内实现健康探测看门狗。

### 阶段 6：测试与构建

- `go test ./tun -v`
- `go test ./...`
- `go build ./...`
- `go vet ./tun`

## 验收标准

- Linux/macOS `tun.Available()` 在支持环境下返回 `true`。
- macOS `route_darwin.go` 完成接口 IP 配置与 up。
- 系统 DNS 可在启动时重定向到 TUN IP，停止时恢复。
- 崩溃/异常退出后 `CleanupResidual` 能恢复路由与 DNS。
- 测试通过，构建成功，不在当前主机启用真实 TUN。

## 风险与缓解

| 风险 | 缓解 |
|---|---|
| 改系统 DNS 后网络彻底断掉 | 外部 watchdog 监控父进程退出并清理；`CleanupResidual` 启动兜底；测试不真改 |
| macOS `ifconfig`/Linux `resolv.conf` 需要 root | `Engine.Start` 已调用 `EnsureAdminPrivileges` |
| Linux 发行版 DNS 管理方式不同 | 优先检测 `systemd-resolved`/`NetworkManager`，回退到 `/etc/resolv.conf` 直接备份 |
| TUN 一旦异常退出导致路由残留 | 各平台 `CleanupResidual` 在下次启动时清理；Windows 外部 watchdog 兜底 |
