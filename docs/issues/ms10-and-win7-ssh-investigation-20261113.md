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
