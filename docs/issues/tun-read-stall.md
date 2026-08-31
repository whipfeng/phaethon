# TUN Read Stall: Wintun.ReceivePacket() 永久阻塞

## 问题概述

TUN 模式启动后约 4-30 秒，`Wintun session.ReceivePacket()` 永久阻塞，不再返回任何数据包。Windows 停止向 TUN 设备发送流量，导致所有通过 TUN 的网络连接中断。

## 症状

1. `tun counters` 日志显示 `read` 计数器停止增长，`write` 计数器继续增长
2. 进程没有崩溃，没有 panic，没有 read error
3. TUN 接口状态正常（Up, Connected）
4. 路由表正常（0.0.0.0/1 + 128.0.0.0/1 via TUN, 198.18.0.0/15 via TUN）
5. `device.Write()` (SendPacket) 仍然正常工作
6. curl/ping/浏览器全部超时

## 复现步骤

1. 启动 phaethon.exe（TUN 模式启用）
2. TUN 初始化成功，开始处理流量（read/write 计数器增长）
3. 约 4-30 秒后，read 计数器停止增长
4. 所有网络连接中断

## 典型日志

```
# 正常工作（启动后 0-4 秒）
[15:51:16] tun engine started on phaethontun
[15:51:16] tun read FAKE: 192.0.2.2 -> 198.18.0.1 (proto=6 len=52 cnt=33)
[15:51:17] tun read FAKE: 192.0.2.2 -> 198.18.0.4 (proto=6 len=52 cnt=52)
[15:51:18] tun read FAKE: 192.0.2.2 -> 198.18.0.5 (proto=6 len=52 cnt=96)
[15:51:19] tun read FAKE: 192.0.2.2 -> 198.18.0.8 (proto=6 len=52 cnt=119)

# 最后一个 read 包
[15:51:20] tun read FAKE: 192.0.2.2 -> 198.18.0.8 (proto=17 len=1278 cnt=181)

# read 卡住，write 继续
[15:51:20] tun counters: read=181 write=44
[15:51:25] tun counters: read=181 write=80
[15:51:30] tun counters: read=181 write=120
# ... read 永远是 181，不再增长
```

## 代码路径

```
readLoop() [engine.go:511]
  └── e.device.Read(readBuf) [engine.go:523]
        └── windowsTUN.Read() [device_windows.go:58]
              └── t.session.ReceivePacket() [device_windows.go:59]
                    └── Wintun DLL: WintunReceivePacket() — 永久阻塞
```

## 环境

- OS: Windows 10.0.26200 (x64)
- Wintun: 0.14 (bundled dll)
- gVisor: 使用的版本见 go.mod
- Go: v24.13.0

## 已排除的原因

1. **不是路由问题**：路由表正常，TUN 接口 Up，split-tunnel 路由 (0.0.0.0/1 + 128.0.0.0/1) 和 Fake-IP 路由 (198.18.0.0/15) 都存在
2. **不是进程崩溃**：进程正常运行，没有 panic/fatal/deadlock
3. **不是 read error**：ReceivePacket 没有返回错误，只是阻塞
4. **不是 Write 端问题**：SendPacket 正常工作，write 计数器持续增长
5. **不是资源耗尽**：内存 ~50MB，handles ~350，正常范围
6. **不是 Fake-IP 地址泄漏**：已移除 per-IP netstack 注册（之前每个 DNS 查询都调用 AddProtocolAddress 但从不调用 Release）
7. **不是 TCP 连接堆积**：已添加 relayWithIdleTimeout（5 分钟空闲超时，用 atomic 时间戳 + Close()，不用 SetReadDeadline）

## 已修复的相关问题

### 1. Fake-IP 地址泄漏（已修复）
- **问题**：每个 DNS 查询都调用 `p.ns.AddProtocolAddress()` 注册 Fake-IP 到 netstack，但 `Release()` 从不调用
- **修复**：移除所有 per-IP netstack 注册，依赖 promiscuous mode 处理包投递
- **文件**：`tun/fakeip.go`

### 2. TCP Forwarder Handler 堆积（已修复）
- **问题**：`util.Relay()` 可能永远阻塞，导致 TCP forwarder handler goroutine 不返回，gVisor endpoint 累积到 maxInFlight(1024) 上限
- **尝试的修复**：`idleTimeoutConn` 包装器，用 `SetReadDeadline`/`SetWriteDeadline` — 导致 gVisor TCP 内部锁竞争，`CreateEndpoint` 对所有新连接超时
- **最终修复**：`relayWithIdleTimeout()` 函数，用 `atomic.Int64` 时间戳 + 独立 watchdog goroutine 调用 `Close()`
- **文件**：`tun/engine.go`

### 3. Watchdog 探测误杀（部分修复）
- **问题**：watchdog 探测用系统 DNS → 被 TUN DNS 劫持 → 返回 Fake-IP → 拨号 Fake-IP 超时 → watchdog 杀主进程
- **当前状态**：watchdog 容忍度已提高（probeInterval=30s, probeFailLimit=10），但探测仍然失败，因为 TUN read 卡住后所有流量都中断

## 当前阻塞点

**Wintun ReceivePacket() 在启动后 4-30 秒永久阻塞。** 这是 Wintun 驱动层面还是 Windows 网络栈层面的问题，尚不清楚。

### 需要调查的方向

1. **Wintun 驱动 bug**：ReceivePacket 是否有已知的阻塞问题？是否需要更新 Wintun 版本？
2. **Wintun 会话配置**：session 创建时是否有参数影响接收行为？
3. **Windows 网络栈**：是否有组策略、防火墙、或其他安全软件干扰 TUN 设备的包投递？
4. **gVisor netstack 反馈**：netstack 是否通过某种方式通知 Wintun 停止发送包？（例如 flow control）
5. **Channel endpoint 容量**：`channel.New(512, 1500, "")` 的 512 是否足够？如果 channel 满了，是否会导致 Wintun 阻塞？
6. **包处理速度**：engine 处理包的速度是否跟不上 Windows 发送包的速度，导致某种背压？

## 关键文件

| 文件 | 作用 |
|------|------|
| `tun/engine.go` | TUN 引擎核心，readLoop/writeLoop/TCP/UDP 处理 |
| `tun/device_windows.go` | Windows TUN 设备封装（Wintun session） |
| `tun/device.go` | TUN 设备接口定义 |
| `tun/fakeip.go` | Fake-IP 池管理 |
| `tun/route_windows.go` | Windows 路由设置 |
| `tun/watchdog_probe.go` | Watchdog HTTP 探测 |
| `main_tun.go` | TUN 主循环和 watchdog |

## 配置

```yaml
# config.yaml
tun:
  enabled: true
admin:
  enabled: true
rules:
  - DOMAIN-SUFFIX,github.com,SOCKS5_7890
  - DOMAIN-SUFFIX,google.com,SOCKS5_7890
  - DOMAIN-SUFFIX,baidu.com,DIRECT
  - MATCH,DIRECT
```
