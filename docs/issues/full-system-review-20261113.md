# 全系统审视（2026-11-13）

> 范围：phaethon 主仓（admin、mesh/p2p、tun、server、dialer、db、升级/部署）。
> 方法：全量单测 + `go vet` + 关键路径代码走查 + 线上 9 节点状态核对。
> 不含：gvisor fork 内部 TCP 栈实现（该 fork 工作区干净、与 origin 同步）。

## 结论摘要

功能面当前稳定：单测全绿，9 个节点统一 `v0.8.0-674`，自升级（成功/失败回滚/缺包/僵尸回收）
已在 VM/QG 实机验证。但审视发现：

- **P0 安全 1 项**：mTLS CA 私钥与 `CN=all` 客户端私钥被提交进 Git，可直接接管所有节点 admin/SSH。
- **P1 稳定性 5 项**：磁盘无界增长（已实际打满 QG）、回滚目标被 retention 删除、
  watchdog 升级日志默认不可见、自升级幂等可能误判、Windows 缺 exe 修复覆盖不全。
- **P2 加固项若干**：无界 sticky cache、mesh 停止路径阻塞、上传整包入内存、公共包端点无认证、
  `go build ./...` 失败、Windows unsafe.Pointer、Win7 工具链、mesh 明文/认证、密钥落盘等。

---

## P0：必须立即处理

### P0-1 mTLS CA 私钥与 `CN=all` 客户端私钥已进入仓库

**证据**

- `admin/admin.go:54`：`//go:embed certs/ca.crt`，该 CA 被所有未显式配置 ClientCA 的节点信任。
- `admin/admin.go:1111`：`if cn == "all" || cn == nodeID {` —— `CN=all` 客户端证书直接放行。
- `git ls-files` 包含：`ca.key`、`client-all.key`、`client.key`、`admin.key`、`tools/client-all.key`。
- `ca.crt` 与 `admin/certs/ca.crt` 哈希相同；`client-all.crt` 由该 CA 签发，
  `client-all.key` 与证书公钥成对匹配。
- `git show --stat 40e4c12` 显示这些私钥是在该提交中加入的（提交信息与密钥无关）。
- 实测：VM/QG/qgw/deskzq/ms9 的 admin 均接受该 CA 签发的 `CN=all` 证书。

**影响**

任何能读到仓库或历史的人都可以：

1. 用 `client-all.crt/key` 通过 mTLS 访问所有节点 admin（无需密码）；
2. 调 `/api/ssh/keys` 添加自己的 SSH 公钥，以 phaethon 运行用户执行命令
   （QG/MB 为 root，JF/GG 为 layer4）；
3. 修改代理/规则/TUN 配置、触发重启、下载/上传包（包仍需签名，但前两项已足够接管主机）。

**处置（需你确认后执行）**

1. 轮换 CA：生成新 CA，签发按节点 CN 的新客户端证书（不再使用 `CN=all`）；
2. 更新 `admin/certs/ca.crt` 并全量自升级（674 已具备自升级能力）；
3. 从 HEAD 与历史中移除 `ca.key`/`client-all.key`/`client.key`/`admin.key`；
   仅删 HEAD 不够——私钥已进入推送历史，必须视为已泄露；
4. `.gitignore` 增加 `*.key`/`*.pem`（保留必要的测试证书例外）；
5. 审计各节点 admin 日志中的 `mTLS auth success ... cn=all` 来源；
6. 考虑取消无条件信任 `CN=all`，改为按节点签发、短期有效。

---

## P1：稳定性与正确性

### P1-1 磁盘无界增长（已实际导致 QG 100%，阻断升级）

**证据**

- `p2p/cache.go:73`（SeedOwnBinary）与 `:216`（StoreFromReader）只写不删；
  QG 实测 `data/cache/p2p-cache` 244 个文件、约 2.6G。
- `admin/package.go:863` 的 `applyAllRetention()` 没有任何调用者；
  `data/packages-pending` 只增不减（QG 实测 340M）。
- `listPackages()` 只清理“有元数据、文件已丢”的条目，不清理“有文件、无元数据”的孤儿文件。
- activation 里的 `go s.applyRetention(...)`（`admin/package.go:409`）是异步的；
  自升级会在约 2s 后杀掉 admin，retention 很可能被截断。

**影响**：包/缓存持续增长，最终 `no space left on device`，升级、日志、DB 写入全部失败。
本次扩散时 QG 已真实发生。

**处置**

1. 启动时执行 `applyAllRetention()`；
2. `apiPackageReceive`/upload 后按 platform/arch 保留 N 版（如 3 版）；
3. BinaryCache 增加按组保留 N 版 + 总大小上限 + 定期清理；
4. 清理孤儿 `.pkg` 与 `.sha256`；
5. 上传前检查剩余空间（低于阈值直接拒绝）；
6. 把 retention 放到自升级成功之后或同步执行（见 P1-2）。

### P1-2 回滚目标会被 retention 删除

**证据**：activate 后 `go applyRetention` 只保留最新 3 版；回滚到更老版本时，
目标包可能在 watchdog 读取前被删除。实测上传 `v0.8.0+badrollback` 后，
watchdog 报 `no signed package for v0.8.0+badrollback`，就是 retention 先删除了包。

**影响**：设计中的“激活旧版本回滚”不可靠，取决于 retention 与 watchdog 的竞态。

**处置**：retention 跳过当前运行版本和 `upgrade-request` 的目标版本；
或先写请求并让 watchdog 完成暂存，再执行 retention。

### P1-3 watchdog 升级日志默认不可见

**证据**：`main.go:1181` 注释说明 watchdog 未配置模块级别时默认 `Error`，
info/warn 被静默；实测 VM 的 `data/logs/phaethon-watchdog.log` 停留在 11-03。
升级成功/回滚主要靠 `upgrade-status` 记录，缺少审计与排障日志。

**处置**：watchdog 启动时默认把 `main` 模块设为 info，或升级事件写独立
`data/logs/upgrade.log`（不受全局级别过滤）。

### P1-4 自升级幂等判断可能误判成功

**证据**：`upgrade_apply.go:315-321`，`cp.workerVersion()` 为空时回退读取
`worker-version` 文件；若子进程正在崩溃循环而文件仍是上次成功版本，
会直接 `markUpgradeSucceeded` 并清掉请求。

**处置**：幂等成功必须同时满足 `cp != nil && cp.ready && cp.workerVersion()==req.Version`；
`worker-version` 只用于展示/状态，不用于判定成功。

### P1-5 Windows 启动器缺 exe 修复覆盖不全

**证据**：VM 的 `start-phaethon.vbs` 已加入“exe 缺失时先恢复 .old”的修复；
qgw/deskzq 未发现 phaethon 相关的计划任务/注册表启动项（`schtasks`/Run 键均无匹配），
其开机自启方式待确认。若同样直接启动 `phaethon.exe`，断电恰好落在两次 rename
之间时无法拉起。

**处置**：确认 qgw/deskzq 的启动方式；为所有 Windows 节点安装带缺 exe 修复的启动器。

---

## P2：加固与工程卫生

| # | 问题 | 证据 | 建议 |
| --- | --- | --- | --- |
| P2-1 | `stickyCache` 无上限 | `mesh/mesh.go:292`、`:1097` 只写不删 | 加上限/LRU；路由重算时清理失效项 |
| P2-2 | mesh 停止路径可永久阻塞 | `mesh/mesh.go:985` 仅 `close(closeCh)`；`:1107`、`:1113`、`:1325` 裸写 `criticalCh` | 发送改 `select { case ch<-...: case <-closeCh: }`；`Stop` 加 `sync.Once`/WaitGroup |
| P2-3 | 上传/接收整包入内存 | `admin/package.go:188`、`:547`；`signing.VerifyPkg` 读完整二进制 | 流式落盘验签；限制并发上传数 |
| P2-4 | 公共包端点无认证 | `admin/admin.go:1058-1072`：GET packages/download、POST receive 公开 | receive 加限速/空间上限；download 可选鉴权 |
| P2-5 | `go build ./...` 失败 | `tools/` 多个 `main`；`gvisorfork` 仅 `_test.go` | tools 分目录或加 `//go:build ignore`；gvisorfork 加 doc.go 或移出 |
| P2-6 | Windows unsafe.Pointer | `tun/route_api_windows.go:435-447` 等硬编码 80 字节行/偏移 | 改用 x/sys 结构或校验表长度 |
| P2-7 | Win7 工具链风险 | qgw 为 Windows 7，运行 Go 1.26 构建（官方已不支持 Win7） | 锁定 Go 版本或规划 OS 升级 |
| P2-8 | mesh 加密/认证可选 | `P2PKey` 为空时帧明文；控制面除底层代理外无认证 | 按路线图启用 PSK/加密；明确威胁模型 |
| P2-9 | 密钥落盘明文 | 代理私钥/密码在 DB/config 明文（D31 曾决定暂缓） | 记录接受风险；后续接 OS keyring |
| P2-10 | admin TLS 未显式 MinVersion；TLS 生成失败会回退 HTTP | `admin/admin.go:704-711`、`:787-790` | `MinVersion=TLS1.2`；未认证时拒绝明文 admin |
| P2-11 | 死代码 | `selfUnparseable()` 无调用；`writeLastGoodWorker` 写入无人读取 | 删除或接入实际逻辑 |
| P2-12 | 本机无法跑 race | 无 gcc/cgo，`-race` 不可用 | Linux CI 跑 `go test -race ./mesh ./p2p ./db` |

---

## 已验证良好的部分

- 单测：`go test` 对主包、admin、config、connlog、db、dialer、frame、mesh、p2p、
  pkg/upgrade、reverse、server、tun、util 全部通过（reverse 约 4 分钟）。
- 主二进制 `go build .` 通过；Linux/Darwin/Windows/Windows7 交叉构建通过。
- 9 节点统一 674；自升级 672→674、假包回滚、缺包不伤服务、僵尸回收均已实机验证。
- 包安全：签名校验、zip 文件白名单、路径穿越校验、大小上限（单包 128MiB/上传 100MB）。
- SSH：仅公钥认证；host key 持久化；公钥删除立即生效。
- 数据面：P2P 每 peer 队列有界（4MB/512 帧/256 队列）；connlog 有界（500/100）。
- 并发：路由表 atomic 快照；Topology 用新 `PeerInfo` 指针发布，避免字段撕裂；
  p2p 清理在 `m.mu` 外串行执行（D9）。

## 建议批次

1. **P0 凭证轮换**：需你确认后执行，涉及全节点 admin 证书与一次全量升级。
2. **P1 稳定性批次**：磁盘清理/retention、回滚保护、watchdog 日志、幂等判定、Windows 启动器。
3. **P2 分批**：先做低风险卫生项（P2-1/2/5/11/12），再做上传流式化与认证加固。
