# 统一跨平台 TUN 实现 + 引擎级看门狗

## 元数据

- 文档类型：Plan
- 版本：v0.1.0
- 所属项目：phaethon
- 创建日期：2026-08-25
- 依赖计划：无

## 背景

TUN 模块当前仅在 Windows 上可用（`tun/avail_other.go` 硬编码返回 `false`），但 Linux/macOS 的设备创建文件已存在。macOS 路由设置缺少接口 IP 与 `ifconfig up`；DNS 目前只在 netstack 内部劫持，没有把系统 DNS 重定向到 TUN；且缺少引擎级健康看门狗，无法在网络被 TUN 搞挂时自动恢复。

本次按 MDD 步骤统一实现 Windows/Linux/macOS TUN，并附加引擎级看门狗。

## 目标

1. 跨平台启用 TUN（Linux/macOS 不再默认不可用）。
2. 补齐 macOS 接口 IP 与路由。
3. 补齐系统 DNS 重定向。
4. 新增引擎级健康看门狗。
5. 统一各平台清理逻辑。
6. 不在当前工作主机启用真实 TUN 整机测试。

## 阶段

### 阶段 1：跨平台可用性开关
- 修改 `tun/avail_other.go`：Linux/macOS 运行时检查 TUN/utun 能力，可用则返回 `true`。

### 阶段 2：macOS 接口与路由
- 在 `tun/route_darwin.go` 中通过 `ifconfig` 配置 utun IP 并 up。
- teardown 时 down 接口。

### 阶段 3：系统 DNS 重定向
- 新增 `tun/dns_system*.go`：
  - Windows: `netsh` 设置/恢复 DNS
  - Linux: `resolv.conf` 备份/恢复，兼容 `systemd-resolved`/`NetworkManager`
  - macOS: `networksetup` 设置/恢复 DNS
- `Engine.Start/Stop` 中调用。

### 阶段 4：引擎级看门狗
- 新增 `tun/watchdog.go`：每 5s 探测 TUN 内部 DNS，连续失败 3 次自动 `Stop()` + `CleanupResidual()`。
- `Engine.Start/Stop` 中集成。

### 阶段 5：统一清理逻辑
- 新增 `tun/cleanup_other.go`：Linux/macOS 删除 split-tunnel 路由、down 接口。
- 更新 `tun/cleanup_windows.go`：增加 DNS 恢复兜底。

### 阶段 6：测试与构建
- `go test ./tun -v`
- `go test ./...`
- `go build ./...`
- `go vet ./tun`

## 验收标准

- Linux/macOS `tun.Available()` 在支持环境下返回 `true`。
- macOS `route_darwin.go` 完成接口 IP 配置与 up。
- 系统 DNS 可在启动时重定向到 TUN IP，停止时恢复。
- 看门狗在 TUN 健康异常时自动停止引擎并清理。
- 测试通过，构建成功，不在当前主机启用真实 TUN。
