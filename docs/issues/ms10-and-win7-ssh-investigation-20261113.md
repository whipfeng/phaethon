# ms10 异常与 Win7 PS2 SSH 调研（2026-11-13）

## 一、ms10（Kubernetes worker3）为什么不稳定

### 现象

- vm/qg/gg/ms9 的 `/api/mesh` 都没有 ms10 的 peer/topology/bestPath；
- `https://ms10.phn` 的 admin/内置 SSH 出现 TLS EOF / connection reset；
- `ssh migu@10.161.88.10` 间歇性 `banner exchange` 超时；从 ms9/QG 也复现；
- admin upload 报 `WinError 10054` / `SSL UNEXPECTED_EOF`，但其他节点用同一套上传代码都成功。

### 定位

ms10 是 Kubernetes 节点 **worker3**，从 ms9（worker4）执行：

```
kubectl get nodes -o wide
worker3   NotReady   control-plane,worker   ...   10.161.88.10

kubectl describe node worker3
Conditions:
  NetworkUnavailable   False   Calico is running on this node
  MemoryPressure       Unknown NodeStatusUnknown  Kubelet stopped posting node status.
  DiskPressure         Unknown NodeStatusUnknown  Kubelet stopped posting node status.
  PIDPressure          Unknown NodeStatusUnknown  Kubelet stopped posting node status.
  Ready                Unknown NodeStatusUnknown  Kubelet stopped posting node status.
Events:
  NodeHasInsufficientMemory / EvictionThresholdMet (x223 over 3h)
  NodeNotReady
```

- kubelet 在 22:22:58 后停止上报，22:28:07 转为 NotReady；
- 节点上大量 Pod 处于 `Terminating`/`Pending`，kubelet API proxy 直接 TLS handshake timeout；
- 主机层面 ms9 ping ms10 仅 0.2ms、QG ping 47ms，Calico 显示 Up，但 sshd/phaethon admin 的 TCP 连接经常只完成握手、不返回 banner。

### 结论

**ms10 的问题是 worker3 节点级资源耗尽/kubelet 卡死，不是 phaethon 的 upload 或 mesh 代码。**

节点内存压力（事件里 `EvictionThresholdMet` 持续 3 小时）导致 kubelet 停止上报、大量 Pod 驱逐，主机上的 sshd/phaethon 也被拖慢或阻塞；mesh 链路因此断开，admin 上传自然 reset。重试上传没有意义，需要先恢复 worker3。

### 附带发现（独立问题）

ms9/gg 的 `/api/p2p` 有失败 peer：

```
socks5: connect to :0 fail: dial tcp :0: connect: connection refused
```

对应代理 `RV_PROXY`：`type=socks5`、`server=""`、`port=0`、`reverse-address=10.161.88.9`、`p2p=true`。P2P manager 把这种反向/占位代理当成主动拨号目标，造成无意义的 `:0` 重连。它不是 worker3 NotReady 的原因，但是一个真实的 P2P 配置/实现缺陷；建议在 `StartPeer` 跳过没有 dial target 的代理（或至少不要对反向代理拨号）。

## 二、Win7 PowerShell 2.0 + winpty 实机调研

### 方法

写了一个独立 Go harness（不替换生产 phaethon），加载部署目录的 `winpty.dll` 0.4.3，通过 winpty 拉起：

```
powershell.exe -NoLogo
```

发送 `Write-Output PS2-OK`，等待 2.5s，再发送 `exit`，观察输出和进程退出；在 qgw（Win7 SP1，PS 2.0）实机运行。

### 结果

```
dll=C:\Users\wangqian\Desktop\phaethon\winpty.dll
spawned pid handle=260 createErr=0
conin=\\.\pipe\winpty-conin-...
conout=\\.\pipe\winpty-conout-...
send: Write-Output PS2-OK
output-after-command:
[0K
---end---
send: exit
wait-after-exit=258   (WAIT_TIMEOUT)
PS2 did not exit on 'exit' within 4s; terminating
```

### 结论

PS2 + winpty **可以 spawn，但交互宿主不可用**：

- 命令没有回显/执行结果，只输出一个 ANSI 擦行序列；
- 发送 `exit` 后 PS2 不退出，必须强制终止。

这与代码里的注释一致：PS2 的交互宿主在 winpty 隐藏控制台下不能正常处理控制台输入/渲染，Win7/8 的 `ptyAvailable()` 门禁是合理的。

### 可行路径

1. **安装 WMF 5.1（PowerShell 5.1）**，把门禁改成“PowerShell >= 3 才允许 winpty”，再实机验证；
2. 只对 `cmd.exe` 放开 Win7 winpty（cmd 是原生控制台宿主，需单独实验）；
3. 维持行级模式，文档标注限制。

本次 harness 和结果已归档到 `archive/tmp-20261113/`，qgw 上的探针包/临时二进制已删除。

### 补充实验：cmd.exe 与 PS2 -Command -

同一 harness 再跑三组（Win7 SP1，随包 winpty 0.4.3）：

| 会话 | spawn | 提示符/回显 | marker 文件 | exit |
| --- | --- | --- | --- | --- |
| `cmd.exe /Q` | 成功 | **完整提示符 + 命令回显** | 写入成功（9 字节） | 正常退出（wait=0） |
| `powershell.exe -NoLogo`（PS2 交互） | 成功 | 只有 `ESC[0K`，无提示符/回显 | **未创建** | 超时不退出（258） |
| `powershell.exe -NoLogo -Command -`（PS2） | 成功 | 打印 PowerShell 帮助：`已指定 -Command 的参数，但尚未重定向标准输入` | 未创建 | 退出 |

结论更新：

- **winpty 版本/wrapper 本身没有问题**：cmd.exe 在 Win7 上通过 winpty 获得完整 PTY（提示符、回显、执行、退出都正常）；
- **问题在 PS2 的交互宿主**：它不处理 winpty 的控制台输入/不渲染，`exit` 也不响应；
- `-Command -` 在 winpty 下不可用：winpty 给子进程的是控制台而不是重定向 stdin，PS2 直接打印帮助并拒绝；
- **嵌套 cmd 也失败**：在 winpty 的 cmd 提示符里手动输入 `powershell.exe -NoLogo`，cmd 本身正常回显，但 PS2 启动后无提示符/输出、marker 未创建、`exit` 超时；`cmd /Q /c powershell.exe -NoLogo` 同样。说明不是“直接 spawn PS2”的启动方式问题，而是 PS2 的 ConsoleHost 在 winpty 模拟控制台下无法进入输入/渲染循环；
- 因此 Win7 想立刻获得真交互，最现实的系统方案是 **cmd.exe + winpty**（不是 PowerShell）；想要 PowerShell 交互则需安装 WMF 5.1/PS7 后再验证。

### 补充实验：官方 winpty 0.4.3 对照

用官方 `winpty-0.4.3-msvc2015` 的 x64 与 x64_xp 构建，替换 bundled DLL/agent，分别测试：

| DLL | spawn flags | PS2 参数 | 结果 |
| --- | --- | --- | --- |
| bundled | 1 | `-NoLogo` | 只有 `ESC[0K`，marker 未创建，exit 超时 |
| 官方 x64 | 1 | `-NoLogo` | 同上 |
| 官方 x64_xp | 1、0、2、3 | `-NoLogo` | 同上 |
| 官方 x64_xp | 1 | `-NoLogo -Sta` | 同上 |

结论：**不是随包 winpty 的构建/版本问题，也不是 spawn flags 问题**。官方 0.4.3（含 XP 兼容构建）与 bundled 表现一致；cmd 在同一个官方 DLL 上仍然正常。可以确认是 **PS2 的 ConsoleHost 与 winpty 的 conhost 模拟不兼容**（winpty 没有完整覆盖 PS2 依赖的 console 行为）。Win7 上没有任何现成 winpty 版本能解决 PS2 的 PTY 交互。

## 三、Shell/PTY 行为决策（2026-11-14）

原则：

1. **指定了程序**：原样使用，不判断程序类型、不自动换程序；必须有 PTY，拿不到 PTY 直接报错。**不引入线级交互**。
2. **未指定程序**：自动探测 shell；无论 PTY 是否可用都必须有兜底：PTY 可用就用系统 shell，不可用就用内置 Bubble Tea Shell。**不引入线级交互**。
3. 内置 Bubble Tea 是 phaethon 自己的交互式 Shell，不是对任意程序的 PTY 包装。
4. `shell` 不带 `pty-req` 是存在的（如 `ssh -T`、程序化 SSH 客户端明确不要 PTY）。

| 请求 | pty-req | PTY 可用 | 指定了程序 | 未指定 |
| --- | --- | --- | --- | --- |
| `exec` | 无 | - | 管道执行命令（非交互） | 管道执行命令（非交互） |
| `exec` | 有 | 是 | PTY 内执行 | PTY 内执行 |
| `exec` | 有 | 否 | **拒绝执行并报错** | **拒绝执行并报错** |
| `shell` | 无 | - | **报错**（指定程序必须有 PTY） | **内置 Bubble Tea** |
| `shell` | 有 | 是 | PTY 内启动指定程序 | PTY 内启动自动探测 shell |
| `shell` | 有 | 否 | **报错** | **内置 Bubble Tea** |

补充：

- `exec + pty-req` 拿不到 PTY 时绝不静默退回管道执行：客户端明确要 TTY，很多程序（sudo/top/ssh）在无 TTY 下行为不同。
- 指定 PS2 时，Win7 的 winpty 无法提供可用 PTY → 直接报错，由用户自己改用 cmd 或安装 WMF 5.1。
- 未指定时，Win7 自动探测优先 cmd（PTY 可用）；若连 cmd/PTY 都不可用，进入内置 Bubble Tea，不会退化成线级。

## 四、v0.8.0-694 实机验证（2026-11-14）

用 x/crypto/ssh 探针（可精确控制 pty-req 的 cols/rows）加真实 `ssh -T`/`ssh -tt` 在 QG、VM、qgw 三套 PTY 实现上逐项验证：

| 场景 | QG（Alpine，Linux pty） | VM（Win11，ConPTY+PowerShell 5.1） | qgw（Win7 SP1，cmd+winpty） |
| --- | --- | --- | --- |
| `exec` 无 pty | ✅ 管道执行 | ✅ | ✅ |
| `exec` + pty 80x24 | ✅ | ✅ | ✅ |
| `exec` + pty 0x0 | ✅ | ✅ | ✅ |
| `shell` + pty | ✅ 系统 shell | ✅ PowerShell 提示符/回显/exit | ✅ cmd 提示符/回显/exit |
| `shell` 无 pty（未指定） | ✅ 内置 Bubble Tea；`help\nexit\n` 逐行执行 | ✅ | ✅ |
| `shell` 无 pty（指定程序） | ✅ 明确报错、exit 1 | ✅ 明确报错、exit 1 | ✅ 明确报错、exit 1 |
| `shell` + pty（指定程序） | ✅ `/bin/sh` | ✅ `cmd.exe` | ✅ `cmd.exe`；PS2 → 明确报错、exit 1 |

验证期间发现并修复的问题：

1. **pty-req 0x0 被错误拒绝**：`ssh -tt` 重定向 stdin、程序化 SSH 客户端会发送 0x0。PTY 本身可用，只是窗口大小未知；改为保留默认 80x24（v692）。
2. **Win7 `exec` + PTY 输出被截断**：winpty 的 `Wait` 提前释放 winpty 对象，conout 在排水前被关闭，客户端只看到清屏序列。释放改到 `CloseIO`/`Kill`（v692）。
3. **Windows 连续两次自升级失败**：第一次升级后 `.old` 是仍在运行的看门狗映像，`rename .old→.old2` 被 Windows 拒绝，节点卡在旧版本；且回滚会落到更老的 `.old`。改为 `.old` 不可轮换时使用 `.old.last`，marker 记录实际备份路径（v693）。VM/qgw 因旧看门狗无法自愈，已用分离式脚本手工引导到 v694。
4. **错误原因不可见**：请求 `Reply(false)` 时 OpenSSH 只显示 "shell request failed"，丢弃 stderr。改为接受请求后写 stderr + exit-status，客户端能看到具体原因（v694）。
5. qgw 的内置 SSH 只登记了 `id_rsa`（cmll）公钥，默认 ed25519 未授权；这是密钥配置问题，不是 SSH 服务缺陷。

注：VM 重启后 `vm.phn` 的 mesh DNS 会短暂 `no such host`，几秒后恢复；与本批 SSH 改动无关。

### 自升级回归

- **694 → 695**：VM、qgw、QG 全部 `succeeded`。VM/qgw 此前因 `.old` 轮换失败卡在 684，已用分离式批处理脚本手工引导到 694；695 是引导后的第一次正常自升级。
- **695 → 696（测试版本 `v0.8.0-696-gfa11bac`）**：VM/qgw 再次 `succeeded`，确认 Windows 节点已恢复连续自升级能力。
- 696 的 swap 会把仍在运行的看门狗映像（694）轮换到 `.old2`。因此下一次升级会命中 `.old2` 被占用、`rename .old→.old2` 失败的条件，正好验证 `.old.last` 回退路径；该路径已有单测覆盖，下一次官方构建发布时做最终实机确认。
