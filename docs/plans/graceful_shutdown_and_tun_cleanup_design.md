# 优雅关闭挂死修复与 TUN 残留清理设计

## 任务信息

- 创建日期：2026-11-04
- 状态：DRAFT → 实现中
- 来源：Mac（darwin）功能测试时发现，根因平台无关

## 背景

2026-11-04 在 Mac 开发机（mb.qg.phn）部署 v0.8.0-288-gaee8ba3 做功能测试，发现两个叠加问题：

1. **优雅关闭必挂死**：SIGTERM 后进程打印 `Received signal terminated, shutting down...` 后永久卡死，只能 kill -9。
2. **TUN 设备残留**：kill -9 后 utun 接口与 100.x 地址残留；且由于问题 1 的存在，实际上**每一次**关闭最终都会演变成 kill -9，残留不可避免。

### 挂死根因（实测 + 代码证据）

SIGQUIT 取证：goroutine 1 卡在 `mesh.(*Netstack).Stop` 的 `n.wg.Wait()`（netstack.go:712）。

- `Netstack.Start` 把 4 个栈 goroutine（acceptTCP/acceptUDP/writeLoop/logPacketCounts）注册进 `n.wg`，这 4 个都监听 `n.closeCh`，`Stop()` close 后会退出。
- **但 `DNSHijacker.Start`（dns.go:534）把 6 个自己的 goroutine（4 个 dnsWorkerLoop + serveLoop + forwarderListener）也通过 `n.WaitGroup()` 注册进了同一个 wg**。它们等待的是 `h.closeCh` / `forwarderStopCh`。
- `Netstack.Stop()` 只 `close(n.closeCh)` 然后 `wg.Wait()`，**全代码没有任何调用方在 Stop 路径上调用 `DNSHijacker.Stop()`**。该方法是完备的（关 forwarder socket、close stopCh、SERVFAIL 收尾、close closeCh、关 udpEP），就是它没人调。
- 结果：wg 计数永远不清零，`wg.Wait()` 永久阻塞。平台无关，只要 mesh netstack 起过（含 mesh-only 节点）就必现。

### 连带影响（老问题得解）

- **QG `rc-service phaethon stop` 卡 `status: stopping`、必须 zap**：OpenRC 发 SIGTERM → phaethon 挂死在 wg.Wait → 超时被 SIGKILL → OpenRC 状态机卡死。与本次根因完全吻合。
- JF/GG/MS9/MS10 一直 pkill -9、VM 一直强杀，从未走优雅路径，所以只有优雅停止路径暴露。

### 残留清理的第二个 gap

`CleanupResidual()` 目前只匹配 **198.18.x.x** 地址才把残留 TUN 接口 down 掉（cleanup_darwin.go / cleanup_linux.go），而 mesh TUN 地址是 **100.x（subnet+2）**，条件永远为假——即使修复挂死，硬死（kill -9/断电/crash）后的残留下次启动也清不掉，utun 会越积越多。

## 目标

1. 优雅关闭在合理时间内（秒级）完成，不再挂死。
2. 硬死后的 TUN 残留（接口 + 地址 + 路由）在下次启动时被可靠清理。

## 关键设计决策

### 决策 1：`Netstack.Stop()` 中先停 DNS hijacker 再 `wg.Wait()`

```go
close(n.closeCh)
// DNS hijacker goroutines share n.wg via WaitGroup(); signal them first.
if n.dnsHijacker != nil {
    n.dnsHijacker.Stop()
}
n.wg.Wait()
```

- `DNSHijacker.Stop()` 自身幂等（stopOnce），重复调用安全。
- 顺序：close(n.closeCh) → hijacker.Stop() → wg.Wait() → ns.Close()。hijacker 的 SERVFAIL 收尾写 udpEP 需要 netstack 还活着，必须在 ns.Close() 之前。
- 为何不在 `close(n.closeCh)` 处让 hijacker 也监听 n.closeCh：hijacker 是独立组件、有自己的 Stop 语义（SERVFAIL 收尾、关 socket），直接复用比让两套关闭通道耦合更清晰。

### 决策 2：`CleanupResidual(tunIP net.IP)` 按真实 TUN 地址匹配

- 签名加 `tunIP` 参数；匹配条件改为 `ip.Equal(tunIP)`（同时保留 198.18/15 旧匹配作为兜底）。
- 匹配后动作：darwin `ifconfig <name> delete <ip>` + `down`（删地址后 split-tunnel 路由随内核自动移除，实测验证过）；linux `ip addr del` + `ip link set down`。
- windows 清理本来就按适配器名（phaethontun）走，不依赖 IP，签名对齐即可（参数不使用）。
- `tunIP` 来源（统一 = mesh subnet +2，与 `mesh.getHostIP()`/fakeip 保留位约定一致）：
  - `main_engine.go` 启动清理：从 `meshMgr.GetSubnet()` 解析 +2 推导（meshMgr 为 startEngine 参数，此时已可用；nil 或解析失败传 nil，退化为只清 198.18/路由）。
  - `manager.go StartTUN` 运行时清理：`Manager.ConfigureMeshAddresses` 已计算过 hostIP（manager.go:198），存为 `e.meshHostIP` 复用。
- 传 nil（如 e2e 测试 `_recreate_check`）时行为与旧版一致（198.18 + 路由清理）。

## 不做的事

- 不改 `platformTeardown` 的 down-only 语义：优雅路径关闭 fd 后由内核回收 utun（问题 1 修复后这条路能走通）。
- 不动 DNS hijacker 内部结构、不动 watchdog 信号转发逻辑。

## 风险

| 风险 | 缓解 |
|------|------|
| hijacker.Stop() 在 Start 失败/半初始化状态被调用 | `stopOnce` + `h.started` 判空保护；netstack.Stop 判 `n.dnsHijacker != nil` |
| 清理误伤其他 VPN 的 utun | 精确 IP 匹配（本节点 subnet+2），不再按网段猜；198.18 旧匹配保留原行为 |
| Linux `tun*` 前缀匹配到 calico tunl0 | tunl0 不会持有本节点 100.x 地址，精确匹配后天然安全 |

## 验收标准

1. Mac 实测：TUN 开启状态下 SIGTERM → 进程秒级退出，watchdog 正常回收。
2. Mac 实测：kill -9 后重启实例，日志出现 cleanup 记录，残留 utun/地址/路由被清。
3. `go build ./...`、`go vet ./tun ./mesh`、`go test ./tun ./mesh` 通过。
4. QG 侧效果（部署后验证）：`rc-service phaethon stop` 不再卡 stopping，zap 不再需要。
