# TUN Watchdog HTTP 连通性探测改造

## 任务信息

- 分支名：`master`
- 目标：将 TUN watchdog 探测从本地 DNS probe 升级为真实 HTTP GET 连通性探测
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
- [x] 保留 `ProbeTUNDNS()` 作为辅助诊断
- [x] 新增 `tun/watchdog_probe_test.go` 单元测试

### Task 2.2: 调整 watchdog 监控逻辑
- [x] `main_tun.go:runWatchdog()` 使用 HTTP probe 替代 DNS probe
- [x] 移除 `dnsGrace` grace period
- [x] 探测间隔改为 3 秒
- [x] 连续失败阈值改为 2 次
- [x] 通过环境变量 `LAYER_WATCHDOG_PROBE_URLS` 传递自定义探测地址

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
- [x] `go test ./tun -v -run TestProbeTUNHTTP` 通过

### Task 3.2: 功能验证
- [x] 正常启动 TUN，日志出现 HTTP probe ok（debug 级别日志在 watchdog log 中）
- [x] `/api/tun` 正确返回 `probeURLs` 默认地址列表
- [x] 配置不可达 `probe-urls` 后，约 6 秒内 watchdog kill 父进程并清理 TUN
- [x] 清理后路由恢复、PhaethonTUN 适配器被删除

### Task 3.3: 更新索引并提交
- [x] 更新 `docs/index.md`
- [x] 标记所有 task 为 [x]
- [x] `git add` 相关文件
- [x] `git commit` 新提交
