# 内建 SSH 无法自适应终端窗口大小

**创建时间**: 2026-11-24
**状态**: ✅ 已修复
**影响范围**: 内建 SSH 所有交互式会话（ConPTY / winpty / 内置 Bubble Tea），全平台

## 现象

本地终端窗口放大/缩小后，内建 SSH 里的 shell 仍按连接建立时的列宽换行：输出行被截断或大面积留白，全屏程序（tmux/vim/PS 进度条）布局错乱。OS sshd（`loc.<node>.phn`）正常，只有内建 SSH 有问题。

## 根因

`server/ssh.go` 的 `handleSession` 用一个 `for req := range reqs` 循环消费 channel 请求。收到 `shell`/`exec` 并启动进程后，代码**直接 return**，请求流从此无人消费：

```go
case "shell", "exec":
    ...
    runSession(channel, proc, true)
    return          // <- reqs 被丢弃
```

RFC 4254 §6.7 规定终端尺寸变化以 `window-change` 请求**在会话生命周期内**持续到达同一个 channel 请求流。循环提前退出后：

- 后续所有 `window-change` 永远排队无人读取，客户端侧不会报错；
- PTY 一直停在 `pty-req` 给出的初始尺寸（默认 80x24）。

所以不是某个 PTY 实现的 resize 失效，而是 resize 事件根本没到 `proc.Resize`。

## 修复

1. **会话级请求消费者**：新增 `serveSessionRequests`，在 shell/exec 启动后接管请求流直到会话结束，把 `window-change` 转给 `proc.Resize`，其余请求按原语义 `Reply(false)`。
2. **安全收尾**：新增 `watchSessionResize`，返回的 stop 函数会等待消费协程真正退出后再返回，然后才执行进程 teardown，避免 late `window-change` 撞上已释放的 PTY 句柄。
3. **内置 Bubble Tea shell**：`runBuiltinShellWithResize` 把 resize 事件转为 `tea.WindowSizeMsg`（容量 1 的通道只保留最新尺寸），fallback shell 与真实 PTY 行为一致；会话结束前先停掉生产者再关通道，杜绝 send on closed channel。
4. **关闭后 resize 防护**：
   - `server/ssh_session_windows.go`：`conptyProc.Resize` 加 `mu` 与 `closed` 检查（原先只有 Close/Kill 有锁）；
   - `server/ssh_session_unix.go`：`ptyProc` 同样加 `mu`/`closed`，避免对已关闭 fd 做 ioctl（fd 复用后可能打到无关文件）；
   - winpty 路径原本已有 `freed` 检查，无需改动。

零尺寸 `window-change` 仍然忽略，保留 pty-req 阶段的 80x24 默认值，与原行为一致。

## 验证

- `TestServeSessionRequestsAppliesWindowChange`：120x40 生效、0x0 忽略。
- `TestServeSessionRequestsStopsWithSession`：stop 后协程退出，无泄漏。
- `TestSSHWindowChangeResizesConPTY`（Windows 实机 ConPTY）：pty-req 80 宽启动 PowerShell，断言初始 `[Console]::WindowWidth == 80`，发送 `WindowChange(40, 120)` 后断言变成 120 —— 证明 resize 真正落到 ConPTY，而不只是通道里少了个报错。
- `go test ./server` 全量通过；`GOOS=linux go build ./server` 通过；`go vet` 仅剩 `winpty_windows.go` 历史遗留的 unsafe.Pointer 警告。

## 备注

- 修复点只涉及请求流消费与 PTY 句柄生命周期，不改变 shell 选择、编码、环境变量等既有行为。
- 客户端（OpenSSH、PuTTY、移动端终端）在窗口变化时自动发送 `window-change`；不发送的客户端（如某些脚本库）仍保持初始尺寸，属协议层正常行为。
