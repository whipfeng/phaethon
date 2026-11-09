# 实机验证清单（2026-11-09 批次 1–6）

> 适用版本：本清单对应的 master（决策 D1–D30 全部落地后）。
> 目的：在 VM / 实机上验证「代码已实现」的功能确实按设计工作。每条给出命令与预期结果。
> 前置：已备份 `data/` 目录；记录基线版本 `curl -s 127.0.0.1:39999/api/version`。

## 0. 通用准备

```bash
# 版本与健康状况
curl -s 127.0.0.1:39999/api/version
curl -s 127.0.0.1:39999/api/mesh | head -c 500
# 日志（默认在 data/ 下，见 docs/LOG_LOCATIONS.md）
tail -f data/*.log
```

## 1. 批次 1：安全止血

| # | 验证点 | 命令 / 操作 | 预期 |
|---|---|---|---|
| 1.1 | 上传包只进 pending、不自动升级 | 上传一个 `.pkg` 后：`ls data/packages-pending/ data/packages/`；看日志 | 新包只在 `packages-pending/`；`data/state/worker-pin` 不变；日志无「自动升级/重启」记录 |
| 1.2 | 手动激活（下次生效） | `POST /api/packages/{id}/activate?restart=later` | 包从 pending 移到 `packages/`，pin 写入新版本；进程不重启 |
| 1.3 | 手动激活（立即生效） | 同上但 `restart=now` | 进程 `os.Exit(0)` 后由 watchdog 拉起；`/api/version` 显示新版本；pin 与运行版本一致 |
| 1.4 | debug API 默认关闭 | `curl -i 127.0.0.1:39999/api/db/proxies` | 404。加 `--enable-debug-api` 重启后：GET 200；`curl -X POST .../api/db/proxies` → 403 |
| 1.5 | 映射监听地址 | 某 mapping 配 `listen-addr: 127.0.0.1`，重启 | `ss -lntp \| grep <port>` 只绑定 127.0.0.1；从另一台机器连该端口失败 |
| 1.6 | bcrypt + 会话失效 | 控制台修改管理员密码；用旧 cookie 访问 `/api/config/admin` | 旧会话返回 401；`data` 中 `admin-config.password` 以 `$2a$/$2b$` 开头；`/api/config/admin` 不再返回明文密码/私钥 |
| 1.7 | 注册端口限制 | 配置端口范围 `0-0` 或 PreferredPort < 1024 | 启动日志拒绝；实际监听端口落在 20000–39999 |

## 2. 批次 2：数据面

| # | 验证点 | 操作 | 预期 |
|---|---|---|---|
| 2.1 | matcher 热更新无并发读写崩溃 | 压测中反复修改规则/代理组（10 次） | 进程不出现 `concurrent map read and map write`；规则即时生效 |
| 2.2 | 内存不随流量增长 | 双节点 mesh 跑 5–10 分钟 iperf3 | `Go heap` 稳定；`/api/mesh` 中 `channelDrops` 不随时间线性增长 |
| 2.3 | 入站会话上限 | 并发建 >64 个 P2P 入站会话 | 第 65 个被拒绝并关闭；`PHAETHON_MAX_P2P_SESSIONS=100` 启动后上限变为 100 |
| 2.4 | 零拷贝正确性 | 大流量内层传输后校验数据完整性（如 iperf3 校验/大于 5MB 文件下载比对 sha256） | 数据无损坏；无 double-free panic |

## 3. 批次 3：TUN 生命周期

| # | 验证点 | 操作 | 预期 |
|---|---|---|---|
| 3.1 | 反复开关 TUN | admin 面板开关 TUN 10 次（间隔 5s） | 每次都能启动；出向流量恢复；无 panic；`disable → enable` 后 netstack→TUN 方向仍有流量 |
| 3.2 | 崩溃恢复（DNS） | 开 TUN → `kill -9` → 重启 | 系统 DNS 恢复为原值（非 DHCP 兜底）；`data/state/linux-dns.json`、`windows-tun.json`、`darwin-dns.json` 使用后删除 |
| 3.3 | 路由残留 | `kill -9` 后重启，检查路由 | Linux：`ip route \| grep 200` 无残留；Windows：`route print` 无 phaethontun 相关默认路由 |
| 3.4 | macOS 抢占恢复 | 先由其它 VPN 建立 0/1 路由，再启动/停止 phaethon | 停止后第三方 0/1 路由被恢复；`darwin-foreign-splits.json` 生成并删除 |
| 3.5 | 平台清理幂等 | 未启用 TUN 时执行清理/启动 | 不误删物理口静态 DNS、不误删其它网卡默认路由 |

## 4. 批次 4：健康检查 / SSH 池 / ctx / 版本协商

| # | 验证点 | 操作 | 预期 |
|---|---|---|---|
| 4.1 | 健康检查实时生效 | 改某组 `health-check-interval: 5`，等待 10s；再删除该组 | 组内成员出现健康数据与延迟；删除后 DB 中不再出现该组（不被复活），日志无该组健康检查 |
| 4.2 | best 组选路 | 配置 best 组含 DIRECT + 两个节点，健康检查 URL 可访问 | DIRECT 延迟非 0（真实探测）；延迟最低的存活成员被选中；REJECT 不参与 |
| 4.3 | SSH 池复用 | 经 SSH 代理连续 100 次短连接 | sshd 侧连接数保持 1（或极少）；client 日志无频繁 `background reconnecting` |
| 4.4 | 版本协商 | 查 `/api/p2p`；再用旧版本节点（或伪造 `minProtocolVersion=11`）对接 | 正常节点显示 `protocolVersion: 10`、`capabilities: [stripe]`；不兼容节点被断开但按周期持续重试（不拉黑） |
| 4.5 | ctx 取消 | 健康检查对不可达节点超时 | 日志出现 `dial cancelled` / `timed out after`，且不残留连接 |

## 5. 批次 5：规则引擎与 DNS

| # | 验证点 | 操作 | 预期 |
|---|---|---|---|
| 5.1 | 非法 CIDR 拒绝保存 | 新增规则 `IP-CIDR,1.2.3.4/33,DIRECT` | HTTP 400，错误含「非法 CIDR」；库中无该条 |
| 5.2 | 未知类型拒绝 | 新增 `FOO,BAR,DIRECT` | 400「未知规则类型」；加载历史数据时该条被跳过并 Warn |
| 5.3 | DOMAIN 生效 | 规则 `DOMAIN,a.example.com,DIRECT`，用 `/api/rules/test` 测试 `a.example.com` 与 `b.a.example.com` | 前者命中，后者不命中 |
| 5.4 | 后缀边界 | `DOMAIN-SUFFIX,example.com,DIRECT` 测试 `evilexample.com` | 不命中（旧实现会误命中） |
| 5.5 | 禁用规则 | 规则加 `//` 前缀 | 不参与匹配；删除/禁用其引用代理不被阻断 |
| 5.6 | DNS 非 A 查询 | 经 TUN 执行 `dig MX gmail.com @<GIP>` | 返回 NODATA（无 A 记录冒充）；A 查询仍返回 FakeIP |
| 5.7 | DNS 防伪造 | 用脚本向转发端口注入错误 TXID/来源的应答 | 日志出现 `dropping unverified response`；缓存未被污染（后续 A 查询结果正确） |

## 6. 批次 6：FakeIP / 写 TUN 优先级 / mesh 不可达 / 包缓存

| # | 验证点 | 操作 | 预期 |
|---|---|---|---|
| 6.1 | FakeIP LRU | 用 /28 小池，连续解析超过容量的域名，再查最早解析的域名 | 最早域名映射被淘汰（`LookupDomain` 返回空），地址被复用；`fakeip` bucket 不增长 |
| 6.2 | FakeIP 落盘不阻塞 | 高 QPS 解析（>500/s）同时观察延迟 | P99 不因 bolt 写事务抖动；无 `failed to persist` 洪泛 |
| 6.3 | 写 TUN 优先级 | TCP 压测（大带宽）同时持续 `dig` | DNS 响应延迟无明显劣化；`/api/mesh` 无持续增长的丢弃 |
| 6.4 | mesh 无路径 ICMP | 断开两节点链路后 `ping <对端 VIP>` | 收到 ICMP 主机不可达；`/api/mesh` 中 `meshUnreachableDrops` 增长；恢复链路后 ping 通 |
| 6.5 | 包缓存上限 | 上传 >128 MiB 包 | 被拒绝（400/500 + 明确错误）；`p2p-cache/` 无残留 `.tmp` |
| 6.6 | 包缓存并发写 | 两个并发请求写同一 key | 最终文件完整，`.sha256` sidecar 与实际文件一致 |

## 7. 已知遗留（验证时可忽略，但不要误判为回归）

1. `tools/` 目录含多个 `package main` 脚本，`go build ./...` / `go vet ./...` 仍会报 `main redeclared`——脚本目录，非产品代码。
2. `reverse/e2e_test.go`、`dialer/reverse_udp_e2e_test.go` 以 `//go:build ignore` 挂起：它们依赖已删除的 `config.RuleConfiguration`，待移植到 `db` + `RuleMatcher`（需要 matcher 重置测试钩子）。反向链路的现有覆盖见 `dialer/reverse_integration_test.go`。
3. `tun/route_api_windows.go` 的 `unsafe.Pointer` vet 警告为既有实现，未改动。
4. D14 健康运行态已外置为进程级注册表，但仍是内存态：进程重启后会重新探测（设计如此，不做持久化）。
5. D5（mTLS 共享 CA）、D22（反向配对串不加密）、D31（代理凭据字段级加密）按决策维持现状/暂缓。
