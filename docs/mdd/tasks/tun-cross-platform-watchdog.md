# 统一跨平台 TUN 实现 + 引擎级看门狗

## 任务信息

- 分支名：`master`
- 目标：统一实现 Windows/Linux/macOS TUN，并附加引擎级看门狗
- 依赖计划：[tun_cross_platform_watchdog.md](../plans/tun_cross_platform_watchdog.md)
- 注意事项：**不在当前工作主机上启用真实 TUN**，避免网络中断

## 阶段 1：跨平台可用性开关

### Task 1.1: 修改 `tun/avail_other.go`
- [x] Linux/macOS `Available()` 运行时检查 `/dev/net/tun` 或 utun control
- [x] 不可用时记录 warn 并返回 `false`，不再硬编码

## 阶段 2：macOS 接口与路由

### Task 2.1: `route_darwin.go` 接口配置
- [x] `platformSetup` 通过 `ifconfig` 设置 utun IP 和 netmask
- [x] `ifconfig devName up` 启动接口
- [x] 保存 `DefaultIfaceName` / `DefaultIfaceIndex`

### Task 2.2: teardown 清理
- [x] `platformTeardown` 删除 split-tunnel 路由后 `ifconfig devName down`

## 阶段 3：系统 DNS 重定向

### Task 3.1: Windows DNS
- [x] 新增 `tun/dns_system_windows.go`
- [x] `setSystemDNS` 调用 `netsh` 设置静态 DNS 为 TUN IP
- [x] `restoreSystemDNS` 恢复 DHCP 或原 DNS

### Task 3.2: Linux DNS
- [x] 新增 `tun/dns_system_linux.go`
- [x] 检测 `systemd-resolved`/`NetworkManager`
- [x] 备份 `/etc/resolv.conf` 并写入 TUN IP，停止时恢复

### Task 3.3: macOS DNS
- [x] 新增 `tun/dns_system_darwin.go`
- [x] 通过 `networksetup -listnetworkserviceorder` 找到活跃服务
- [x] 设置/恢复 DNS 服务器

### Task 3.4: Engine 集成
- [x] `Engine.Start()` 成功后调用 `setSystemDNS`
- [x] `Engine.Stop()` 中先恢复 DNS 再 teardown 路由

### Task 3.5: DNS 重定向测试
- [ ] 集成测试需真实 TUN/VM 环境，当前主机不启用真实 TUN；代码中 DNS 重定向为平台 exec 调用，由 watchdog 健康检查间接覆盖

## 阶段 4：引擎级看门狗

### Task 4.1: `tun/watchdog.go`
- [x] 定义 `HealthWatchdog` 结构
- [x] 每 5s 探测 TUN 内部 DNS
- [x] 连续失败 3 次触发保护

### Task 4.2: 保护动作
- [x] 调用 `engine.Stop()`
- [x] 调用 `CleanupResidual()`
- [x] 记录错误日志

### Task 4.3: Engine 集成
- [x] `Engine.Start()` 末尾启动看门狗
- [x] `Engine.Stop()` 第一步停止看门狗

## 阶段 5：统一清理逻辑

### Task 5.1: Linux/macOS 清理
- [x] 更新 `tun/cleanup_linux.go`
- [x] 更新 `tun/cleanup_darwin.go`
- [x] 删除 `0.0.0.0/1`、`128.0.0.0/1` 路由
- [x] Linux: `ip link set <iface> down`
- [x] Darwin: `ifconfig <iface> down`

### Task 5.2: Windows 清理增强
- [x] `tun/cleanup_windows.go` 增加恢复 DNS 为 DHCP

## 阶段 6：验证

### Task 6.1: 单元测试
- [x] `go test ./tun -v`
- [x] `go test ./...`

### Task 6.2: 构建
- [x] `go build ./...`
- [x] `GOOS=linux go build ./...`
- [x] `GOOS=darwin go build ./...`
- [x] `go vet ./tun`

## 阶段 7：提交

### Task 7.1: 正常提交
- [ ] `git add` 相关文件
- [ ] `git commit` 新提交（不 amend）
- [ ] 推送（用户指示不着急 push，待后续网络恢复再推）
