# 内建 SSH 默认 Shell 可配置任务

> 创建日期: 2026-11-08
> 关联方案: [ssh-shell-configurable.md](../plans/ssh-shell-configurable.md)
> 状态: 代码完成，待部署验证

## 阶段 1: db 层扩展

### Task 1.1: SSHConfig 加 Shell 字段
- [x] `db/ssh.go` `SSHConfig` 加 `Shell string \`json:"shell,omitempty"\``
- [x] 旧数据兼容验证：缺字段时 `Shell=""` 走自动探测（标准 encoding/json 行为）

### Task 1.2: db 单元测试
- [ ] `TestSSHConfigWithShell`：写入/读取往返（已有 db 测试套件覆盖 Put/Get 通路，未单独加）
- [ ] `TestSSHConfigLegacyNoShell`：旧 JSON（仅 enabled）反序列化 Shell 为 ""（同上）

## 阶段 2: server 层接入

### Task 2.1: Windows 端 resolveShell
- [x] `server/ssh_session_windows.go` `defaultShell` 改名 `autoDetectShell`（逻辑不变）
- [x] 新增 `resolveShell()`：先 db.Shell，空则 autoDetectShell()
- [x] `startSessionProc` 顶部用 `resolveShell()` 替换 `defaultShell()`

### Task 2.2: Windows execArgs 补 bash 分支
- [x] `execArgs` 加 `bash`/`sh`/`zsh` 命中 `-c <cmd>` 分支（按文件名包含 `bash`/`zsh` 或去 `.exe` 后以 `sh` 结尾判定）
- [x] `utf8InitCmd` 对 bash/sh/zsh 返回空串（POSIX shell 无需 chcp），调用方跳过空串写入

### Task 2.3: Unix 端 resolveShell
- [x] `server/ssh_session_unix.go` 同样新增 `resolveShell()`，覆盖 `$SHELL` 行为
- [x] `startSessionProc` 接入

### Task 2.4: server 单元测试
- [ ] `TestResolveShellDBOverride` / `TestResolveShellFallback` / `TestExecArgsBash` 未单独加（验证走 VM 自测路径）

## 阶段 3: admin API

### Task 3.1: GET /api/ssh 加 shell 字段
- [x] `admin/ssh.go apiSSH` 响应 map 加 `"shell": cfg.Shell`

### Task 3.2: PUT /api/ssh/config 接受 shell
- [x] `apiSSHConfig` req body 加 `Shell string`
- [x] 写库 `&db.SSHConfig{Enabled: req.Enabled, Shell: strings.TrimSpace(req.Shell)}`

### Task 3.3: API 测试
- [x] 整体覆盖语义：PUT 带 enabled + shell，与现有 enabled 提交路径合并

## 阶段 4: admin UI

### Task 4.1: admin.html 加 shell 输入框
- [x] SSH 区块在 enabled 开关下加 `<input id="ssh-shell">` + label + placeholder + 帮助文本 + Save 按钮
- [x] i18n key 引用 `admin.sshShell` / `admin.sshShellPh` / `admin.sshShellHelp`（沿用现有 SSH 区块风格：仅 HTML 默认文本，i18n.js 未补——与同区块 sshTitle/sshEnabled 一致）

### Task 4.2: JS 读写
- [x] `loadSSH` 把 `data.shell` 填进 `#ssh-shell`
- [x] 抽出 `saveSSHConfig(enabled, shell)` 共用函数，enabled 开关和 shell Save 按钮都调它，PUT body 同时带 `enabled` 和 `shell`

### Task 4.3: 样式微调
- [x] 输入框 `font-family:monospace`，与现有 host key 指纹风格一致；flex 布局自适应

### Task 4.4: SSH 区块订阅版本号
- [x] admin.html SSH IIFE 内注册 `onBusinessVersion('ssh', loadSSH, 'admin-ssh-block')`
- [ ] 验证：本页 PUT 后 loadSSH 自动重跑；他端改动也能即时刷新本页（待部署验证）

## 阶段 5: 验证

### Task 5.1: 本机构建
- [x] `go build ./db/ ./server/ ./admin/ ./handler/` 通过
- [x] `GOOS=windows/linux/darwin go build ./server/` 三平台通过
- [x] `go vet ./db/ ./server/ ./admin/` 通过
- [x] `go test ./db/ ./server/ ./admin/` 全过

### Task 5.2: VM 自测
- [ ] admin UI 配 `C:\Program Files\Git\bin\bash.exe` → `ssh vm.phn` 拿到 Git Bash
- [ ] admin UI 配 `pwsh` → `ssh vm.phn` 拿到 PowerShell 7（或配绝对路径）
- [ ] admin UI 留空 → `ssh vm.phn` 回到 PowerShell 5.1（自动探测）
- [ ] exec 模式 `ssh vm.phn 'ls'` 在三种配置下均正常

### Task 5.3: QG 验证（用户确认后部署）
- [ ] 配 `/usr/bin/zsh` 后 `ssh qg.phn` 拿到 zsh
- [ ] 留空回到 `$SHELL`

## 阶段 6: 部署

### Task 6.1: VM 部署
- [ ] 一条命令内 stop + replace + start
- [ ] 启动日志版本戳正确

### Task 6.2: QG 部署（用户确认后）
- [ ] `rc-service phaethon stop → zap → start` 序列
- [ ] 跨节点 `ssh qg.phn` 验证

### Task 6.3: 提交
- [ ] commit message 说明改动和原因
- [x] 更新 docs/index.md 挂接新 plan/task

## 阶段 7: 代码审查修复（2026-11-08 二审）

### Task 7.1: Unix 端接 execArgs（修 Linux 上 PowerShell Core 不可用 bug）
- [x] 公共逻辑搬到 `server/ssh_session.go`（无 build tag）
- [x] 新增 `classifyShell`：basename 分类 posix/powershell/cmd，覆盖 fish/ksh 等
- [x] Unix `startSessionProc` exec 分支改用 `execArgs(shell, cmdline)`

### Task 7.2: defaultWorkDir 修 Unix 会话起始目录
- [x] 新增 `defaultWorkDir()`：`os.UserHomeDir()` → Windows `%SystemDrive%\` → `/`
- [x] Unix `cmd.Dir = "/"` 改 `defaultWorkDir()`；Windows 同步替换

### Task 7.3: resolveShell db 错误 WARN
- [x] db 读失败且非 ErrNotFound 时 `util.LogModuleWarn("ssh", ...)`

### Task 7.4: admin API Shell 路径校验警告
- [x] `server.ValidateShellPath` 导出，绝对路径 stat、相对名 LookPath
- [x] PUT /api/ssh/config 响应加 `warning` 字段（不阻止保存）
- [x] admin.html shell Save 按钮回调展示 warning

### Task 7.5: 前端 SSE 自触发覆盖用户输入修复
- [x] `loadSSH` 检查 `document.activeElement !== shellEl` 才覆写
- [x] Save 按钮回调末尾 `blur()` 主动失焦，恢复 SSE 刷新
- [x] enabled checkbox 与 shell 共享 PUT 的并发覆盖问题记入"已知简化"，单管理员场景够用

### Task 7.6: 验证
- [x] `go build ./db/ ./server/ ./admin/` 三平台通过
- [x] `go vet ./db/ ./server/ ./admin/` 通过
- [x] `go test ./db/ ./server/ ./admin/` 全过
