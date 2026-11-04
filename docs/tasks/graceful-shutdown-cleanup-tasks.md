# 优雅关闭挂死修复 + TUN 残留清理任务

## 任务信息

- 目标：修复 Netstack.Stop 挂死（DNS hijacker goroutine 无人通知退出）；CleanupResidual 按真实 TUN 地址（subnet+2）匹配清理残留
- 创建日期：2026-11-04
- 依赖计划：[graceful_shutdown_and_tun_cleanup_design.md](../plans/graceful_shutdown_and_tun_cleanup_design.md)

## 阶段 1: 修复优雅关闭挂死

### Task 1.1: mesh/netstack.go — Stop() 先停 DNS hijacker
- [ ] `Stop()` 在 `close(n.closeCh)` 之后、`n.wg.Wait()` 之前调用 `n.dnsHijacker.Stop()`（nil 判空）
- [ ] 注释说明 wg 共享来源（dns.go Start 经 WaitGroup() 注册）

### Task 1.2: 编译验证
- [ ] `go build ./...` 通过

## 阶段 2: CleanupResidual 按真实 TUN 地址匹配

### Task 2.1: tun/manager.go — 存储 hostIP 并传递
- [ ] `ConfigureMeshAddresses` 把已计算的 hostIP 存为 `e.meshHostIP`
- [ ] `StartTUN` 中 `CleanupResidual()` → `CleanupResidual(e.meshHostIP)`

### Task 2.2: main_engine.go — 启动清理推导 tunIP
- [ ] 新增 `residualTUNIP(meshMgr)` helper：subnet 解析 +2，失败/nil 返回 nil
- [ ] `startEngine` 中 `tun.CleanupResidual(...)` 传入推导值

### Task 2.3: tun/cleanup_darwin.go — 真实地址匹配 + 删地址
- [ ] 签名改 `CleanupResidual(tunIP net.IP)`
- [ ] 匹配条件加 `tunIP != nil && ip4.Equal(tunIP.To4())`（保留 198.18/15）
- [ ] 命中后先 `ifconfig <name> delete <ip>` 再 `down`

### Task 2.4: tun/cleanup_linux.go — 同上
- [ ] 签名与匹配条件同步改造
- [ ] 命中后 `ip addr del <ip>/32 dev <name>` + `ip link set <name> down`

### Task 2.5: tun/cleanup_windows.go — 签名对齐
- [ ] 签名改 `CleanupResidual(tunIP net.IP)`（内部逻辑不变，按适配器名清理）

### Task 2.6: 调用方同步
- [ ] `tests/e2e/tun/_recreate_check/main.go` 两处调用传 nil

### Task 2.7: 编译/vet/测试
- [ ] `go build ./...`、`go vet ./tun ./mesh`、`go test ./tun ./mesh` 通过

## 阶段 3: 实测验证（Mac）

### Task 3.1: 优雅关闭验证
- [ ] 构建 darwin 二进制，起测试实例，PATCH 开 TUN，SIGTERM → 进程秒级退出，无挂死

### Task 3.2: 残留清理验证
- [ ] kill -9 制造残留 → 重启实例 → 日志出现 cleanup 记录，utun 地址/路由被清

## 阶段 4: 提交

- [ ] 按仓库提交规范分阶段提交（修复 + 清理可分开或合并为一个 commit，message 说明根因）
- [ ] 推送 origin/master
