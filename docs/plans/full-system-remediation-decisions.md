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
| 7 | 看门狗缺失窗口与可靠性 | 应用侧：Linux 改为硬链接/复制 `.old` 后原子 `rename(new, exe)`；Windows 先做 `ReplaceFile` 实验，可行则用，否则保留 rename；ready 前连续失败回退 `.old`；启动修复按 `.old→.old2→.new` 依次尝试并验证；`.new`/marker fsync；升级中检查 `stopping`；`upgrade-status` 写入重试；启动器/自启归运维。**补充（v693）**：`.old2`（轮换目标）是仍在运行的看门狗映像时，Windows 拒绝删除/覆盖，`rename .old→.old2` 因目标仍存在而失败；改为保留 `.old`、用 `.old.last` 作为本次备份，marker 记录实际备份路径 |
| 8 | stickyCache 无上限 | 有界 LRU，上限 65536 条 |
| 9 | mesh 停止路径阻塞 | `Stop` 加 `sync.Once` + WaitGroup；所有 `criticalCh` 写入改为 `select closeCh`；停止时等待 worker 退出 |
| 10 | 上传/接收整包入内存 | 流式落盘后验签；并发上传上限 2；配合剩余空间检查 |
| 11 | 包相关公共端点 | 保持 `list/download/receive` 公开；`receive` 加每源并发/速率限制和剩余空间检查；list/download 只读 |
| 12 | 构建卫生 | `tools/*.go` 加 `//go:build ignore`；`gvisorfork` 加 `doc.go`；临时文件移入归档目录并加入 `.gitignore` |
| 13 | Windows unsafe.Pointer | 优先改用 `x/sys/windows` 官方 MIB 结构；没有封装则保留手写解析并加边界/长度校验 |
| 14 | qgw Windows 7 工具链 | 归构建/运维；应用只更新支持说明（Win7 为 best-effort） |
| 15 | mesh 明文与可选加密 | 保持 `P2PKey` 可选；控制台/P2P 页面显式提示“当前 mesh 未加密”；文档写清威胁模型。**已被第 23 项取代** |
| 16 | 敏感数据明文落盘 | 沿用 D31 保持明文；补启动检查（DB/config 不能全局可读）并记录接受风险 |
| 17 | admin TLS | 显式 `MinVersion: TLS1.2`；TLS 初始化失败且未认证时拒绝启动或只绑 loopback，不允许明文 + 无认证 |
| 18 | 升级相关死代码 | 删除 `selfUnparseable` 与 `last-good-worker` 写入；用第 7 项的 `.old` 回退替代 |
| 19 | race/CI | 增加 Linux CI：`CGO_ENABLED=1 go test -race ./mesh ./p2p ./db` |
| 20 | Win7 内嵌 SSH 交互式访问 | 先调研，结论见第二节；暂不实施 |
| 21 | SSH Shell/PTY 交互规则 | 指定程序必须 PTY，拿不到报错；未指定自动探测，PTY 不可用回内置 Bubble Tea；交互式 shell 不引入线级；`exec` 请求了 PTY 但拿不到时拒绝执行。**已实施并在 v694 实机验证**（QG/VM/qgw），结果见 [ms10-and-win7-ssh-investigation-20261113.md](../issues/ms10-and-win7-ssh-investigation-20261113.md) 第四节 |
| 22 | 新旧 watchdog 交接 | 升级后由旧 watchdog 启动新 watchdog，等新 worker ready 后旧进程退出；失败则 abort + 杀进程树 + 回滚。不兼容没有交接代码的旧版本（回滚到这类版本会失败并自动回滚）。首个版本发布后需对每个节点做一次 watchdog 进程重启以激活。**已实施并在 QG/VM/qgw 实机验证（699 激活，700 交接成功）**。详见 [watchdog-handoff.md](watchdog-handoff.md) |
| 23 | P2P 密钥模型（取代第 15 项） | **取消全局 `p2p-key` 概念**（配置字段、启动注入、控制台警告全部移除）。UDP leg 复用所属代理链路自身凭据派生 AEAD：h_tunnel 用密码原文；Trojan 两端统一用 `SHA224(password)`（与服务端认证值一致）；SOCKS5 配置了密码则复用、未配置则明文。帧层不再做全局 PSK 包装，主载体与 lane 由载体协议自身保护。**已实施并单测覆盖（随下一版本发布）** |

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

**实机探针结果（2026-11-13）**：已用独立 winpty harness 在 qgw 复现——PS2 能 spawn，
但不处理/不渲染控制台输入，`exit` 也不退出（WaitForSingleObject 超时），确认 PS2+winpty
不可用，代码门禁合理。补充实验：**cmd.exe + winpty 在 Win7 上完整可用**（提示符/回显/
执行/退出正常）；在 cmd 里再启动 PS2（以及 `cmd /c powershell`）同样无输出、marker 未创建，
排除“直接 spawn”问题；`-Command -` 在 winpty 下会因 stdin 非重定向而打印帮助。官方
winpty 0.4.3 msvc x64/x64_xp、flags 0/1/2/3、`-Sta` 全部同样失败，排除版本/build/flags
问题。结论：PS2 的 ConsoleHost 与 winpty conhost 模拟不兼容；Win7 想真交互可先用 cmd 或
装 WMF 5.1。详见
[ms10-and-win7-ssh-investigation-20261113.md](../issues/ms10-and-win7-ssh-investigation-20261113.md) 第二节。

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

- [x] 批次 1（安全）
  - [x] 第 1 项：Git 历史清理完成 —— 移除私钥/DB/`.bak`/测试包/已编译工具二进制；`master` 与 tags 已 force-push；mb 已 reset 到新历史；旧历史备份在 `archive/sensitive-20261113/pre-rewrite-20261113.bundle`
  - [x] 第 2 项：`db.InitDefault` 强制 `AuthEnabled=true`；非交互首次启动生成随机密码，写入 `data/state/initial-admin-password`（0600），bcrypt 存库
  - [x] 第 17 项：admin TLS 显式 `MinVersion=TLS1.2`；TLS 不可用且未认证时 admin 仅绑 `127.0.0.1`
  - [x] 第 16 项：启动时把 DB/legacy config 收紧到 0600（Unix）
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
- [x] 批次 4：代码卫生/加固
  - [x] 第 8 项：`stickyCache` 改为有界 LRU（65536）
  - [x] 第 9 项：`Stop` 加 `sync.Once`+WaitGroup；`criticalCh` 写入 select closeCh
  - [x] 第 12 项：`tools/*.go` 加 `//go:build ignore`；`gvisorfork/doc.go`；Windows-only `cmd/ifdiag`、`cmd/iftype` 加 build tag；临时文件已归档到 `archive/tmp-20261113/`（gitignored）
  - [x] 第 13 项：unicast 表改用 `x/sys/windows` 官方结构；neighbor/route 表保留手写解析但加 count 上限（x/sys 未封装对应类型）
  - [x] 第 15 项：启动 warning + `/api/p2p` `encrypted` 字段 + proxies 页面未加密横幅
  - [x] 第 19 项：Makefile 增加 `test-race`（Linux CI 入口）
- [ ] 批次 5：SSH Shell/PTY 规则 + 内置 Bubble Tea（第 21 项）

> 历史清理推迟到最后的理由：避免多次改写/force-push，并让代码修改先完成、验证后再一次性清理。

## 六、部署验证

- 683 已手动部署到 vm、qg；683→684 自升级验证完成：
  - VM 使用 ReplaceFileW，QG 使用硬链接 `.old` + 原子 rename；`upgrade.log` 逐步记录；
  - 首次 684 包因“构建与签名并行”导致包内仍是 683 二进制，watchdog 版本校验
    失败并自动回滚到 683（意外验证了回滚路径）；重新顺序构建/签名后成功；
  - jf/gg/mb/ms9/qgw/deskzq 通过 upload+activate 到 684；ms10 的 mesh 管理
    上传不稳定，改用 OS SSH 手动替换；vm/qg 已 684；
  - 最终 9 节点全部 `v0.8.0-684-g6958a6e`，admin/mesh/内置 SSH 基础功能正常；
  - 多数节点的 supervisor 仍是 674（worker 已是 684），下次服务重启更新。

### v0.8.3-2-gbc45876 全量部署（2026-11-19，第 23 项）

- 构建 4 个签名包：linux/amd64、windows/amd64、windows7/amd64、darwin/amd64；
 版本 `v0.8.3-2-gbc45876`（第 23 项提交后的 `git describe`）。
- 先灰度 vm、qg：upload+activate 自升级均 `succeeded`；随后按 mb → ms9 → ms10 →
  gg → jf → qgw → deskzq 顺序全量，9 节点全部 `v0.8.3-2-gbc45876`、`status=succeeded`。
- 验证：
  - 拓扑 9 节点 / 22 条边；deskzq↔jf、qgw↔jf/mb、gg↔ms10 双边齐全；
  - vm↔qg 的 SOCKS5 UDP leg 保持 `transport=udp`、`phase=active`，in/out 持续增长、
    `drops=0`（该链路密码为空，按设计为明文 leg）；所有节点 `meshInboundDrops=0`、
    `tunWriteDrops=0`；qg 的 jf peer 在 jf 重启瞬间累计 334 次 DataDrops，重启后不再增长；
  - VM/QG 出网 204、DNS `git.migu.cn → 100.135.0.11` 正常；控制台标题带 nodeID。
- 说明：当前在用的 UDP leg（vm↔qg、qg 的 MB_S5）密码为空，因此本次实机未触发
 非空密钥的 leg 加解密路径；该路径由 `TestP2PLegSecretPerProxy` 与
  `TestUDPLegCryptoRoundTrip/WrongSecretDrops` 覆盖。后续任一链路启用非空密码 +
  `p2p-udp` 时，两端升级到本版本即可自动加密。

### 拓扑可视化改进部署（2026-11-19，v0.8.3-10-gd493c9d）

- 拓扑交互：滚轮缩放（光标锚点）、空白拖拽/中键平移、适应画布、定位本机/任意节点、
  重置布局；空白拖拽优先平移，曲线调整改为 Shift+拖拽。
- 布局：本机居中的椭圆分层环；环内按邻居角序排列，再对有界旋转组合搜索
  “交叉数 + 最小间距”。当前 9 节点实测 0 交叉、最小节点间距约 141px。
- 指标：每个方向各一个标签（22 条有向链路 → 22 个标签），标签沿曲线候选点 +
  法向偏移避让；实测标签互相 0 重叠、与节点 0 重叠。画布只显示
  `#seq srtt loss`，完整 retx/cost/friendly 在悬停浮层中双向展示。
- 升级提示：服务重启横幅移除关闭按钮（只保留“立即刷新”）；升级请求 toast
  不再带关闭按钮，避免用户误以为关闭即取消。
- 部署：构建 linux/windows/windows7/darwin 四个签名包，9 节点经 upload+activate
  全部升级到 `v0.8.3-10-gd493c9d`、`status=succeeded`；拓扑 9 节点/22 条边，
  vm↔qg UDP leg 保持 `udp/active`、`drops=0`。

### 发送缓冲/warm lane 自适应部署（2026-11-19，v0.8.3-19-gba787fa）

- 根因：Windows 无 `TCP_NOTSENT_LOWAT`，`SetWriteBuffer` 创建值 16KB，单 lane 被卡在
  ~4.4Mbps（16KB/29ms）；不是物理链路（裸 OS 上传约 15Mbps）。
- 代码：平台下限 64KB；`lowat=2×BDP(baseRTT)`，baseRTT 下限 10ms；增长立即、收缩 3s 后
  每次 25%，并对 primary+所有动态 lane 生效；流式载体默认 2 条 warm lane（UDP leg 活跃时抑制）。
- 不再需要用户调参：`PHAETHON_P2P_LOWAT` / `PHAETHON_WARM_LANES` 仅 `PHAETHON_LAB=1`
  时作为实验覆盖，生产忽略；deskzq 上的临时环境变量已移除。
- 负结果：WS warm lane 提到 3（primary+3）反而更差（四流全部 ~200–240KB/s，首屏 7.9s），
  已回退到 warm=2；说明瓶颈是共享外层队列，不是 lane 数。
- 部署：9 节点全部 `v0.8.3-19-gba787fa`、`status=succeeded`；拓扑 9 节点/22 条边，
  vm↔qg UDP leg `udp/active`、`drops=0`；elephant 首屏 6.0s、缓存 3.7–4.6s。
- 剩余：4 个满速大文件并发仍有单流落后；异步整形（lane 级、非阻塞写循环）作为独立课题，
  见 [lane-async-pacing.md](lane-async-pacing.md)。
- 收尾（2026-11-19）：裸 OS 4/8 流上限 ~2.9MB/s，mesh 约 70–85%；warm3、app pacing、
  UDP leg、lane fairq、128KB lowat、外层 AQM 均无显著收益或负收益（详见
  [elephant-download-dribble.md](../issues/elephant-download-dribble.md) 第七节）。
  当前保留 64KB 自适应 lowat + warm2；剩余差距来自内层 TCP 终止 + 外层载体双层开销，
  继续优化需架构级改动，暂停。deskzq 为 `v0.8.3-29-gcdb2991`（64KB，lab 功能关闭），
  其余节点 `v0.8.3-19-gba787fa`，默认行为一致。

### ms10（worker3）异常定位

- 现象：admin 上传 TLS reset、`ms10.phn` SSH reset、OS SSH banner 超时、所有节点无 ms10 的
  mesh peer/bestPath。
- 根因：worker3 在 Kubernetes 中 **NotReady**（`Kubelet stopped posting node status`），
  事件显示持续 `NodeHasInsufficientMemory` / `EvictionThresholdMet`（3 小时内 223 次），
  大量 Pod Terminating/Pending，kubelet API proxy TLS 超时；主机 sshd/phaethon 被资源耗尽拖垮。
- 结论：**不是上传/mesh 代码问题**，需要先恢复 worker3（释放内存/重启 kubelet 或节点）。
- 附带：ms9/gg 存在 `RV_PROXY`（server 空、port 0、reverse-address 非空、p2p=true）被 P2P
  当成主动拨号目标，产生 `socks5: connect to :0` 失败 peer，建议后续在 `StartPeer` 跳过无 dial
  target 的代理。
- 详见 [ms10-and-win7-ssh-investigation-20261113.md](../issues/ms10-and-win7-ssh-investigation-20261113.md)。

## 七、SSH Shell/PTY 交互规则（第 21 项）

原则：

1. **指定了程序**：原样使用，不判断程序类型、不换程序；必须有 PTY，拿不到 PTY 直接报错。不引入线级交互。
2. **未指定程序**：自动探测 shell；无论 PTY 是否可用都必须有兜底：PTY 可用用系统 shell，不可用用内置 Bubble Tea Shell。不引入线级交互。
3. 内置 Bubble Tea 是 phaethon 自己的交互式 Shell，不是对任意程序的 PTY 包装。
4. `shell` 不带 `pty-req` 存在（`ssh -T`、程序化 SSH 客户端明确不要 PTY）。

| 请求 | pty-req | PTY 可用 | 指定了程序 | 未指定 |
| --- | --- | --- | --- | --- |
| `exec` | 无 | - | 管道执行命令（非交互） | 管道执行命令（非交互） |
| `exec` | 有 | 是 | PTY 内执行 | PTY 内执行 |
| `exec` | 有 | 否 | **拒绝执行并报错** | **拒绝执行并报错** |
| `shell` | 无 | - | **报错**（指定程序必须有 PTY） | **内置 Bubble Tea** |
| `shell` | 有 | 是 | PTY 内启动指定程序 | PTY 内启动自动探测 shell |
| `shell` | 有 | 否 | **报错** | **内置 Bubble Tea** |

- 指定 PS2：Win7 winpty 无法提供可用 PTY → 直接报错，用户自行改用 cmd 或安装 WMF 5.1。
- 未指定：Win7 自动探测优先 cmd（PTY 可用）；cmd/PTY 都不可用 → 内置 Bubble Tea，不退化成线级。
- 实施状态：待开发（批次 5）。



