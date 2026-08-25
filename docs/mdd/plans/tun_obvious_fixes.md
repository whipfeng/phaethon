# TUN 显而易见问题修复计划

## 元数据

- 文档类型：Plan
- 版本：v0.1.0
- 所属项目：phaethon
- 创建日期：2026-08-25

## 背景

` tun/engine.go`、`tun/device_windows.go` 等文件已实现基于 gvisor netstack 的 TUN 流量拦截。代码 review 中发现几个不需要大改架构、但容易出问题的明显缺陷：

1. `readLoop` 把所有包都按 IPv4 注入 netstack，IPv6 包会被错误解析。
2. `readLoop` 复用同一个 2048 字节 buffer，存在数据被覆盖的风险。
3. `proxyDesc` 对 DIRECT 的判断因为大小写不一致而失效。

本次修复只针对这 3 个显而易见的问题，**不开启真实 TUN 进行整机测试**（避免把当前工作网络搞挂）。真实 TUN 拦截测试留到后续在独立 VM/测试机上进行。

## 目标

1. 让 `readLoop` 正确区分 IPv4/IPv6 并注入对应协议层。
2. 让 `readLoop` 每包使用独立 buffer，避免共享内存被覆盖。
3. 修复 `proxyDesc` 对 DIRECT 代理的显示判断。
4. 保持现有公共 API 不变，所有 tun 单元测试继续通过。

## 阶段 1: readLoop 修复

### Task 1.1: IPv4/IPv6 协议识别
- 读取 TUN 包后，根据 IP 头第一个字节的高 4 位判断版本：
  - `0x40` (4) → IPv4
  - `0x60` (6) → IPv6
- 分别调用 `linkEP.InjectInbound(ipv4.ProtocolNumber, pkt)` 或 `linkEP.InjectInbound(ipv6.ProtocolNumber, pkt)`。
- 非 IPv4/IPv6 包直接丢弃。

### Task 1.2: 每包独立 buffer
- 在 `readLoop` 里每次读到数据后，复制到新的 `make([]byte, n)` 再交给 `buffer.MakeWithData`。
- 替代当前复用 `buf := make([]byte, 2048)` 的方案。
- 为减少 GC 压力，可引入 `sync.Pool`（可选，本次先保证正确性）。

## 阶段 2: proxyDesc 大小写修复

### Task 2.1: 统一大小写比较
- `proxyDesc` 中使用 `strings.EqualFold(p.Type, config.ProxyDIRECT)` 替代 `p.Type == config.ProxyDIRECT`。
- 因为配置初始化时 `proxy.Type = strings.ToLower(proxy.Type)`，直接等于大写常量会永远失败。

## 阶段 3: 验证

### Task 3.1: 单元测试
- `go test ./tun -v` 必须全部通过。
- `go test ./...` 无回归。

### Task 3.2: 代码静态检查
- `go build ./...` 成功。
- `go vet ./tun` 无新增告警。

## 验收标准

- `readLoop` 正确处理 IPv4/IPv6。
- `readLoop` 不再复用单 buffer。
- `proxyDesc` 对 DIRECT 显示为 `"DIRECT"`。
- `go test ./...` 通过，`go build ./...` 成功。
- 不在当前工作主机上启用真实 TUN。
