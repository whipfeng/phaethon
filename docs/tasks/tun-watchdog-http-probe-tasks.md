# TUN Watchdog HTTP 连通性探测改造

## 任务信息

- 分支名：`master`
- 目标：将 TUN watchdog 探测统一为真实 HTTP GET 连通性探测，废弃 DNS probe fallback
- 创建日期：2026-08-27
- 依赖计划：[tun_watchdog_http_probe_design.md](../plans/tun_watchdog_http_probe_design.md)

## 阶段 1: 文档与准备

### Task 1.1: 创建计划文档
- [x] 创建 `docs/plans/tun_watchdog_http_probe_design.md`
- [x] 创建本任务文件

## 阶段 2: 后端实现

### Task 2.1: 新增 HTTP 探测函数
- [x] `tun/watchdog_probe.go` 新增 `ProbeTUNHTTP(timeout, probeURLs)`
- [x] 支持多地址 fallback，任一成功即算通过
- [x] 请求附带随机 query 参数绕过缓存
- [x] ~~保留 `ProbeTUNDNS()` 作为辅助诊断~~ → 已废弃，watchdog 统一使用 HTTP probe
- [x] 新增 `tun/bind_*.go` 平台相关接口绑定实现
- [x] 新增/更新 `tun/bind_test.go` 接口绑定测试

### Task 2.2: 调整 watchdog 监控逻辑
- [x] `main_tun.go:runWatchdog()` 使用 HTTP probe 替代 DNS probe
- [x] 移除 `dnsGrace` grace period
- [x] 探测间隔改为 3 秒
- [x] 连续失败阈值改为 2 次
- [x] 通过环境变量 `LAYER_WATCHDOG_PROBE_URLS` 传递自定义探测地址
- [x] 通过环境变量 `LAYER_WATCHDOG_TUN_IFINDEX` 传递 TUN 接口索引用于 socket 绑定

### Task 2.3: 增加配置字段
- [x] `config/config.go` 的 `TUNConfig` 增加 `ProbeURLs []string`
- [x] 为空时使用默认候选地址列表
- [x] `conf/default.yaml` 增加 `tun.probe-urls: []`

### Task 2.4: Admin API 暴露探测状态
- [x] `admin/admin.go` `/api/tun` GET 返回增加 `probeURLs`
- [x] probe 失败次数由 watchdog 进程写入自身日志，供排查使用

## 阶段 3: 验证与收尾

### Task 3.1: 本地验证
- [x] `go build ./...` 成功
- [x] `go vet ./tun` 无警告
- [x] `go test ./tun` 通过

### Task 3.2: 功能验证
- [x] 正常启动 TUN，日志出现 HTTP probe ok（debug 级别日志在 watchdog log 中）
- [x] `/api/tun` 正确返回 `probeURLs` 默认地址列表
- [x] TUN 不可用时，约 6 秒内 watchdog kill 父进程并清理 TUN
- [x] 清理后路由恢复、PhaethonTUN 适配器被删除

### Task 3.3: 后续待修复
- [ ] 修复 Windows 下 TUN 适配器在禁用/清理后未能重新创建的问题

### Task 3.4: 更新索引并提交
- [x] 更新 `docs/index.md`
- [x] 标记所有 task 为 [x]
- [x] `git add` 相关文件
- [x] `git commit` 新提交

## 阶段 4: 纯 Go DNS 解析器与 API 扩展

### Task 4.1: Watchdog DNS 改用纯 Go 实现
- [x] `tun/watchdog_probe.go` 使用 `net.Resolver{PreferGo: true}` 替代 `net.DefaultResolver`
- [x] 分离 DNS 和 HTTP 超时参数：`ProbeTUNHTTPWithBind(dnsTimeout, httpTimeout, ifIndex, probeURLs)`
- [x] `main_tun.go` 定义 `dnsTimeout=5s`、`httpTimeout=8s`
- [x] 更新 `watchdog_probe_test.go` 适配新签名
- [x] 设计文档新增 3.8 节记录方案

### Task 4.2: Admin API 新增 stats 字段
- [x] `tun/fakeip.go` 新增 `FakeIPStats` 结构体和 `Stats()` 方法
- [x] `tun/engine.go` 新增 `TUNStats` 结构体和 `Stats()` 方法（包计数器 + FakeIP 统计）
- [x] `main.go:buildTUNStatus()` 返回中增加 `stats` 字段
- [x] 更新设计文档 4.3 节和验收标准

### Task 4.3: 验证
- [x] `go build ./...` 成功
- [x] `go vet ./tun` 无警告
- [x] `go test ./tun` 通过
