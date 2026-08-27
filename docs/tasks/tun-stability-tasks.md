# TUN 稳定性修复

## 任务信息

- 分支名：`master`
- 目标：修复 TUN 稳定性和资源泄漏问题
- 创建日期：2026-08-27
- 依赖计划：[tun_stability_design.md](../plans/tun_stability_design.md)

## 阶段 1: 文档与准备

### Task 1.1: 创建计划文档
- [x] 创建 `docs/plans/tun_stability_design.md`
- [x] 创建本任务文件

## 阶段 2: 后端实现

### Task 2.1: Linux TUN 设备名修复
- [x] `device_linux.go:41` 截断 null 字节：`strings.TrimRight(string(ifr.name[:]), "\x00")`

### Task 2.2: Engine.Start() 竞态修复
- [x] `engine.go:106-193` 在整个初始化过程中持有 `mu` 锁
- [x] 初始化成功时设置 `running = true`，失败时保持 `false`

### Task 2.3: UDP relay idle timeout
- [x] `engine.go:590-620` 为 `relayUDP()` 两端 Read 添加 30 秒 deadline
- [x] 使用 `SetReadDeadline()` 设置超时

### Task 2.4: Fake-IP Release 清理 netstack 地址
- [x] `fakeip.go:131-138` 在 `Release()` 中调用 `RemoveAddress()`
- [x] 简化 `Release()` 中的地址清理逻辑

### Task 2.5: 删除死代码
- [x] 删除 `dns_cache.go` 整个文件（`DNSCache` 从未被调用）
- [x] 删除 `route_windows.go:393-403` 的 `verifyStaticNeighbor()` 函数
- [x] 删除 `dns_system_linux.go:114-121` 的 `isSystemdResolvedActive()` 函数

## 阶段 3: 验证

### Task 3.1: 本地验证
- [x] `go build ./...` 成功
- [x] `go vet ./tun` 无警告

### Task 3.2: 更新索引
- [x] 更新 `docs/index.md` 添加本计划链接
- [x] 标记所有 task 为 [x]

## 阶段 4: 提交

### Task 4.1: 提交代码
- [ ] `git add` 相关文件
- [ ] `git commit` 新提交
