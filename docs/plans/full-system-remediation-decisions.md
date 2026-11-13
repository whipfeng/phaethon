# 全系统审视决策记录（2026-11-13）

> 来源：[full-system-review-20261113.md](../issues/full-system-review-20261113.md)
> 状态：已逐项决策，待实施。本文件只记录结论，不代表已改代码。

## 一、决策清单

| # | 项目 | 决策 |
| --- | --- | --- |
| 1 | mTLS CA/客户端私钥泄露 | 清理 Git 历史中的敏感数据和垃圾文件；**CA 暂不轮换**（接受风险，后续再议） |
| 2 | 默认 admin 监听 | 保持 `0.0.0.0`；首次初始化强制 `AuthEnabled=true`，生成随机初始密码或一次性 setup token |
| 3 | 磁盘无界增长 | 启动全量 retention；active/pending 每 platform/arch 保留 3 版；p2p-cache 保留 5 版且总量上限 1GB；清理孤儿 pkg/sha256；上传前剩余空间 <512MB 拒绝；retention 与升级流程串行化 |
| 4 | 回滚目标被 retention 删除 | retention 保护当前运行版本、`upgrade-request` 目标版本、最近一次成功版本；放在升级成功后或启动时执行 |
| 5 | watchdog 升级日志不可见 | 新增 `data/logs/upgrade.log`，不受模块级别过滤，逐步记录 request/applying/staging/stop/swap/ready/rollback/cleanup |
| 6 | 自升级幂等误判 | 幂等成功只认实时进程：`cp.ready && cp.workerVersion()==req.Version`；`worker-version` 文件仅用于展示 |
| 7 | 看门狗缺失窗口与可靠性 | 应用侧：Linux 改为硬链接/复制 `.old` 后原子 `rename(new, exe)`；Windows 先做 `ReplaceFile` 实验，可行则用，否则保留 rename；ready 前连续失败回退 `.old`；启动修复按 `.old→.old2→.new` 依次尝试并验证；`.new`/marker fsync；升级中检查 `stopping`；`upgrade-status` 写入重试；启动器/自启归运维 |
| 8 | stickyCache 无上限 | 有界 LRU，上限 65536 条 |
| 9 | mesh 停止路径阻塞 | `Stop` 加 `sync.Once` + WaitGroup；所有 `criticalCh` 写入改为 `select closeCh`；停止时等待 worker 退出 |
| 10 | 上传/接收整包入内存 | 流式落盘后验签；并发上传上限 2；配合剩余空间检查 |
| 11 | 包相关公共端点 | 保持 `list/download/receive` 公开；`receive` 加每源并发/速率限制和剩余空间检查；list/download 只读 |
| 12 | 构建卫生 | `tools/*.go` 加 `//go:build ignore`；`gvisorfork` 加 `doc.go`；临时文件移入归档目录并加入 `.gitignore` |
| 13 | Windows unsafe.Pointer | 优先改用 `x/sys/windows` 官方 MIB 结构；没有封装则保留手写解析并加边界/长度校验 |
| 14 | qgw Windows 7 工具链 | 归构建/运维；应用只更新支持说明（Win7 为 best-effort） |
| 15 | mesh 明文与可选加密 | 保持 `P2PKey` 可选；控制台/P2P 页面显式提示“当前 mesh 未加密”；文档写清威胁模型 |
| 16 | 敏感数据明文落盘 | 沿用 D31 保持明文；补启动检查（DB/config 不能全局可读）并记录接受风险 |
| 17 | admin TLS | 显式 `MinVersion: TLS1.2`；TLS 初始化失败且未认证时拒绝启动或只绑 loopback，不允许明文 + 无认证 |
| 18 | 升级相关死代码 | 删除 `selfUnparseable` 与 `last-good-worker` 写入；用第 7 项的 `.old` 回退替代 |
| 19 | race/CI | 增加 Linux CI：`CGO_ENABLED=1 go test -race ./mesh ./p2p ./db` |
| 20 | Win7 内嵌 SSH 交互式访问 | 先调研，结论见第二节；暂不实施 |

## 二、第 20 项调研：Win7 内嵌 SSH 交互式访问

**当前行为**

- `server/ssh_session_windows.go:33-41`：`ptyAvailable()` 要求 `RtlGetVersion().MajorVersion >= 10`；Win7/8 直接返回 false。
- Win7 上 `pty-req` 被拒绝，客户端退回管道模式；`interactiveShellArgs()` 对 Win7 PowerShell 使用 `-NoLogo -Command -`，只有行级命令执行，没有服务端提示符/本地回显。
- 实测 qgw（Windows 7 SP1）：
  - `$PSVersionTable.PSVersion` = **2.0**；
  - `ssh -tt qgw.phn` 返回 `PTY allocation request failed on channel 0`；
  - 部署目录已有 `winpty.dll`（279552 字节）和 `winpty-agent.exe`（361472 字节）。

**代码现状**

- `server/winpty_windows.go` 已完整实现 winpty 0.4.3 的 PTY 路径，且包含 Win7 兼容代码：
  - `legacyExit` / `scanExitLine`：PS2 不响应 `exit` 时由服务端终止；
  - `winptyInitCmd()` 对 Win7 使用简化的 UTF-8 编码设置，避免 `chcp` 卡死。
- 但 `ptyAvailable()` 的 Win7/8 门禁把这条路径关掉了。注释给出的原因：PowerShell 2.0 的交互宿主在 winpty 下不可靠（有时什么都不显示就退出、`exit` 不退出），而管道回退稳定。

**结论**

- Win7 的“不可交互”不是 winpty 缺失，而是 **PS 2.0 + winpty 的组合不可靠**，代码主动禁用了该路径。
- 可行路径：
  1. **安装 WMF 5.1（PowerShell 5.1）**，然后把 `ptyAvailable()` 的版本门禁改为“PowerShell 主版本 >= 3 时允许 winpty”；这是最干净的方案（需运维安装系统组件）。
  2. **仅对 cmd.exe 放开 Win7 winpty**：cmd 的交互宿主是原生的，可能稳定；需要实机实验验证。
  3. 维持现状：Win7 继续行级模式，文档标注限制。
- 暂不实施；后续按第 1 条在 qgw 上做一次实验（安装 WMF 5.1 → 临时放开 gate → `ssh -tt` 验证 → 决定是否合入）。

## 三、Git 历史清理清单（第 1 项）

先从 mirror/备份开始，再改写历史：

- 私钥：`admin.key`、`ca.key`、`client-all.key`、`client.key`、`tools/client-all.key`
- 运行库：`phaethon.db`
- 垃圾：所有 `*.bak`/`*.bak2`、`test-package.pkg`、`tools/` 下已编译的 `migrate_*`/`restore-*` 二进制
- 保留：`admin/certs/ca.crt`（内嵌 CA）、公开证书、winpty 第三方二进制、docs

历史改写会变更 commit hash，需要 force-push；完成后所有协作者重新 clone。后续实现提交会自然把 `git describe` 版本号继续往上推。

## 四、实施批次

1. 安全：Git 历史清理 + admin 认证 B + TLS/权限检查
2. 升级可靠性：第 7 项 + P1-2/3/4 + 死代码 + upgrade.log
3. 磁盘/包：P1-1 + 流式上传 + 包端点限流
4. 代码卫生/加固：sticky LRU、mesh Stop、构建卫生/归档、unsafe.Pointer、mesh 加密提示、race CI

> 本文件只记录决策；实施时按批次单独提交，并在每批完成后回填状态。

## 五、实施进度

- [x] 批次 1（安全，代码部分）
  - [x] 第 2 项：`db.InitDefault` 强制 `AuthEnabled=true`；非交互首次启动生成随机密码，写入 `data/state/initial-admin-password`（0600），bcrypt 存库
  - [x] 第 17 项：admin TLS 显式 `MinVersion=TLS1.2`；TLS 不可用且未认证时 admin 仅绑 `127.0.0.1`
  - [x] 第 16 项：启动时把 DB/legacy config 收紧到 0600（Unix）
- [ ] 第 1 项：Git 历史清理（放到所有代码批次之后做单次 rewrite + force-push）
- [x] 批次 2：升级可靠性
  - [x] 第 7 项：Linux 改为硬链接 `.old` + 原子 `rename(new, exe)`；Windows 使用 `ReplaceFileW`（Win10/Win7 实测可替换运行中的 exe），失败回退 rename；ready 前连续 3 次失败回退 `.old`；启动修复按 `.old → .old2 → .new` 并校验最小大小；`.new`/marker fsync；升级中检查 stopping；状态写入重试
  - [x] 第 4 项：retention 保护当前运行版本、请求目标、最近成功版本、本次激活版本；activation 时同步执行
  - [x] 第 5 项：新增 `data/logs/upgrade.log`，记录 request/applying/staged/swap/ready/rollback/repair
  - [x] 第 6 项：幂等成功只认 `cp.ready && cp.workerVersion()==req.Version`
  - [x] 第 18 项：删除 `selfUnparseable`、`last-good-worker` 写入；用 `.old` 回退替代
- [x] 批次 3：磁盘/包
  - [x] 第 3 项：启动执行全量 retention；active/pending 每 platform/arch 保留 3 版；孤儿 pkg/陈旧 tmp 清理；上传/接收前检查剩余空间（<512MB 拒绝）；`BinaryCache.Prune(5, 1GiB)` 启动清理
  - [x] 第 10 项：上传/接收改为流式落盘 + `signing.VerifyPkgMeta` 流式验签；并发上限 2
  - [x] 第 11 项：包端点保持公开；receive 加每源 500ms 限流 + 并发/空间检查
- [ ] 批次 4：代码卫生/加固

> 历史清理推迟到最后的理由：避免多次改写/force-push，并让代码修改先完成、验证后再一次性清理。

