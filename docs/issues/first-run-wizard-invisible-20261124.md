# 首次启动初始化向导"不逐个提问"（2026-11-24）

## 现象

全新 Windows 主机双击 `phaethon.exe` 首次启动：

- 能看到进入交互初始化，编码正常；
- 但问题不是一条一条出现、等待输入，而是零散/一次性地出现；
- 用户只能一路回车，最后用一个随机 node-id、`127.0.0.1:39999`、无认证
  的默认配置完成了初始化。

## 根因

### 1. 向导跑在 worker 里，而 worker 的 stdout 是按行转发的管道

首次初始化在 worker 的 `run()` 中执行，但 worker 是由 supervisor（watchdog）
用 `spawnChildProcess` 拉起的：

- worker `stdout` 是 watchdog 的 `os.Pipe()`（用于 `{"ready":true}` /
  `{"heartbeat":true}` 协议消息）；
- watchdog 用 `bufio.Scanner` 按行读取，非协议行再加换行转发到自己的 stdout
  （`readProtocol` → `fmt.Fprintln(os.Stdout, line)`）。

而向导的提示是 `fmt.Print("...: ")`，**不带换行**。这些提示会一直留在管道里，
只有等后续某条带换行的输出（如错误提示或"初始化完成"）才被整体冲出来。
结果就是用户看不到当前问题，回车只是把输入喂给下一个看不见的问题。

### 2. 读输入的 error 被丢弃，EOF 被当成"全部回车"

旧实现所有 `reader.ReadString('\n')` 的 error 都用 `_` 忽略，并直接
`TrimSpace`。stdin 一旦 EOF（或无控制台），每个问题都立刻得到空字符串，
于是一路采默认值、随机生成 node-id，静默完成。

### 3. 叠加风险：向导停留超过 120s 会被 watchdog 杀掉

`childReadyTimeout = 120s`。worker 在向导阶段不会发 `ready`，用户慢慢填写
超过两分钟就会被判"卡死"重启，进入新一轮向导。

## 修复

1. **首次初始化上移到 supervisor**：新增 `prepareFirstRun(workDir)`，在
   watchdog 拉起 worker 之前执行。watchdog 持有真正的控制台，提示可以直接
   显示。顺序为：拿实例锁 → 打开 DB → `IsEmptyConfig` → 交互/默认初始化 →
   关库 → 再 spawn worker。有活跃实例持有 data 目录时跳过初始化。
2. **向导本身**：
   - 改为编号选择（监听地址 1/2/3）+ 每问独立成段，问题与默认值清晰可见；
   - EOF/Ctrl+Z 时返回"输入流已结束，初始化已取消（未写入任何配置）"，
     绝不静默采用默认值；
   - 读密码的错误同样向上传播，不再死循环；
   - 向导失败且 stdin 是终端时提示"按回车键退出"，避免双击窗口一闪而过。
3. **worker 兜底**：worker 侧再遇到空配置只做明确告警 + 默认初始化，不再
   尝试在协议管道里跑向导。
4. **测试**：新增 `db/init_interactive_test.go`
   - 四个问题按 1/2/3/4 顺序消费答案并落库；
   - 立即 EOF、以及答完第一题后 EOF，都必须中止且不写入配置。

## 验证

- `go build ./...` 通过；`go test ./db/ -run TestInteractiveInit` 通过。
- 非交互集成：临时目录 + `< NUL` 启动新二进制
  - watchdog 日志出现 initial admin 凭据写入；
  - `data/phaethon.db`、`data/state/initial-admin-password` 生成；
  - worker 日志直接显示"数据库已加载"，未再次进入初始化。
- 交互集成（ConPTY 探针，模拟双击的新控制台）：逐题看到 `[1/4]`～`[4/4]`
  后依次作答，四题各自等待输入；完成后 `data/phaethon.db` 中可检索到
  `probe-node` 与 `127.0.0.1:39999`，说明答案按题落库，而不是一路回车。

## 备注

- 自启路径（计划任务 → `start-phaethon.vbs`，隐藏窗口且 `< NUL`）仍然按
  非交互初始化：默认 `0.0.0.0:39999` + 随机初始密码文件。这是预期行为，
  因为那条路径没有可交互的控制台。
- 交互向导只在"双击运行且 stdin 是真实终端"时出现。
