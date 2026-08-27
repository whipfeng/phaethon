# TUN 路由与 Fake-IP 修复

## 任务信息

- 分支名：`master`
- 目标：修复 TUN 路由清理顺序、Fake-IP 注册/释放状态一致性，以及 weak-host 验证严格性问题
- 创建日期：2026-08-27
- 依赖计划：[tun_route_and_fakeip_fixes.md](../plans/tun_route_and_fakeip_fixes.md)

## 阶段 1: 文档与准备

### Task 1.1: 创建计划文档
- [x] 创建 `docs/plans/tun_route_and_fakeip_fixes.md`
- [x] 创建本任务文件

## 阶段 2: 后端实现

### Task 2.1: 修复 Fake-IP 注册失败 race
- [x] `tun/fakeip.go:registerIPLocked()` 将 `registered[ipStr] = true` 移到 `AddProtocolAddress()` 成功之后
- [x] 注册失败时只打印 warn，不写入 registered

### Task 2.2: 调整 Stop() 路由清理顺序
- [x] `tun/engine.go:Stop()` 将 `routeMgr.Teardown()` 提前到 `device.Close()` 之前
- [x] 保持 DNS 恢复、DNS proxy 停止在 device close 之前

### Task 2.3: weak-host 验证降级为警告
- [x] `tun/route_windows.go:ensureWeakHostEnabled()` 中 `getWeakHostState()` 失败或验证不通过时只 warn，不返回 error
- [x] `setWeakHost()` 本身失败仍返回 error

### Task 2.4: 修复 FakeIPPool.Release 状态一致性
- [x] `tun/fakeip.go:Release()` 仅在 `RemoveAddress()` 成功后 `delete(registered, ipStr)`
- [x] 失败时保持 registered 状态并打印 warn

### Task 2.5: Start() 错误路径等待 dnsHijack goroutine
- [x] `tun/engine.go` 中所有调用 `dnsHijack.Stop()` 的错误路径后追加 `e.wg.Wait()`
- [x] 确保 serve goroutine 在关闭 netstack/device 前已退出

## 阶段 3: 验证

### Task 3.1: 本地验证
- [x] `go build ./...` 成功
- [x] `go vet ./tun` 无警告
- [x] `go test ./tun` 通过

### Task 3.2: 更新索引
- [x] 更新 `docs/index.md` 添加本计划与任务链接
- [x] 标记所有 task 为 [x]

## 阶段 4: 提交

### Task 4.1: 提交代码
- [x] `git add` 相关文件
- [x] `git commit` 新提交
