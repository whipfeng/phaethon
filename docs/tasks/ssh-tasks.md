# SSH 代理改进

## 任务信息

- 分支名：`master`（单提交仓库，直接 amend）
- 目标：提升 SSH dialer 健壮性，补充测试、超时、缓存生命周期和 ssh-agent 支持
- 依赖计划：[ssh_improvements.md](../plans/ssh_improvements.md)
- 测试目标：通过 SSH 代理访问 `http://106.13.183.103:39998/proxies`

## 阶段 1: 测试补齐

### Task 1.1: 私有函数可测化
- [x] 保留现有公共 API
- [x] 通过 mock SSH server 进行集成测试，不侵入生产代码

### Task 1.2: 新增 `dialer/ssh_test.go`
- [x] 密码认证成功
- [x] 私钥认证成功
- [x] stale client 清缓存后重试成功
- [x] 无认证方法返回错误

### Task 1.3: 运行测试
- [x] `go test ./dialer -run TestSSH`
- [ ] `go test -race ./dialer`（本机无 gcc，cgo 不可用，未执行）

## 阶段 2: SSH handshake 超时

### Task 2.1: 添加握手 deadline
- [x] 默认 15s 超时
- [x] 握手成功后清除 deadline

### Task 2.2: 验证超时
- [x] 单元测试 mock hang 住的 server

## 阶段 3: SSH client 缓存生命周期

### Task 3.1: 带时间戳的缓存结构
- [x] `lastUsed` 记录
- [x] 默认 idle 5 分钟清理

### Task 3.2: 懒清理
- [x] `getSSHClient` 时清理过期条目
- [x] 关闭 client 时避免影响正在使用的连接

### Task 3.3: 验证
- [x] 单元测试模拟过期

## 阶段 4: ssh-agent 认证支持

### Task 4.1: 读取 `SSH_AUTH_SOCK`
- [x] 使用 `x/crypto/ssh/agent`
- [x] 作为私钥/密码之后的 fallback

### Task 4.2: 验证
- [x] 手动在有 ssh-agent 的环境验证

## 阶段 5: 文档更新

### Task 5.1: 更新 `docs/mdd/specs/protocol_spec.md`
- [x] 补充 SSH 认证方式说明
- [x] 说明 UDP 不支持及原因

### Task 5.2: 更新 `README.md`
- [x] SSH 行说明更新

## 阶段 6: 验证与推送

### Task 6.1: 本地验证
- [x] `go test ./...` 无回归
- [x] `go build ./...` 成功
- [x] 使用 `conf-test-ssh/config.yaml` 访问 `http://106.13.183.103:39998/proxies`

### Task 6.2: 推送
- [ ] amend 并 force-push（待用户确认后执行）
