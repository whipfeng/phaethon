# TUN 显而易见问题修复

## 任务信息

- 分支名：`master`
- 目标：修复 TUN 代码 review 中发现的 3 个明显问题
- 依赖计划：[tun_obvious_fixes.md](../plans/tun_obvious_fixes.md)
- 注意事项：**不在当前工作主机上启用真实 TUN**，避免网络中断

## 阶段 1: readLoop 修复

### Task 1.1: IPv4/IPv6 协议识别
- [x] `readLoop` 根据 IP 版本字段选择 `ipv4.ProtocolNumber` 或 `ipv6.ProtocolNumber`
- [x] 非 IP 包丢弃

### Task 1.2: 每包独立 buffer
- [x] 不再复用单个 `buf := make([]byte, 2048)`
- [x] 每包复制到独立 slice 后注入 netstack

## 阶段 2: proxyDesc 大小写修复

### Task 2.1: DIRECT 判断
- [x] 使用 `strings.EqualFold` 比较 `proxy.Type` 与 `config.ProxyDIRECT`

## 阶段 3: 验证

### Task 3.1: 单元测试
- [x] `go test ./tun -v`
- [x] `go test ./...`

### Task 3.2: 构建
- [x] `go build ./...`
- [x] `go vet ./tun`

## 阶段 4: 提交

### Task 4.1: 正常提交
- [x] `git add` 相关文件
- [x] `git commit` 新提交（不 amend）
- [x] 推送（已推送到 origin/master）
