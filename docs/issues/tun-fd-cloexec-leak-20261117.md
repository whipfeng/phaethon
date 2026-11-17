# TUN fd 被子进程继承导致残留 tunN / DNS GIP 路由错位

日期：2026-11-17  
状态：已修复并部署（Linux/macOS v0.8.1-94-gb20b1c4）

## 现象

- qg 作为旁路网关，`nslookup git.migu.cn` 超时；`nslookup git.migu.cn 100.0.0.3` 同样超时。
- `ip -br addr` 同时存在 `tun0` 和 `tun1`，两者都带 `100.0.0.2/29`。
- `ip route get 100.0.0.3` 选择 **tun0**，但当前 netstack 实际使用的是 **tun1**。
- DNSHijacker 日志能正常处理来自远端 GIP 的查询，但收不到 `100.0.0.2 → 100.0.0.3:53` 的本机查询。
- 重启越多，残留 tunN 越多，表现为 DNS 超时、部分 mesh 流量卡顿。

## 证据

```
# ip -br addr | grep tun
tun0  UNKNOWN  100.0.0.2/29 ...
tun1  UNKNOWN  100.0.0.2/29 ...

# ip route get 100.0.0.3
100.0.0.3 dev tun0 src 100.0.0.2 uid 0

# fd 持有者（修复前）
/proc/10953 (watchdog)  1 个 /dev/net/tun
/proc/10961 (worker)    1 个 /dev/net/tun
/qg_leg_stream_watch.sh 及其 sleep 子进程也持有 /dev/net/tun
```

## 根因

1. `open("/dev/net/tun")` 未设置 `CLOEXEC`。
2. 内置 SSH、Watcher 等子进程继承了 TUN fd；worker 退出/重启时旧 fd 仍被这些子进程持有，旧 `tunN` 不随 worker 销毁。
3. 新 worker 通过 `tun%d` 只能拿到 `tunN+1`；旧接口继续保留同一 `/29` 路由。
4. 内核选路可能选中旧接口（如 `100.0.0.3 → tun0`），而当前 netstack 绑定的是 `tun1`，导致 DNS GIP / mesh 流量进入失效 TUN。

## 修复

- `tun/device_linux.go`、`tun/device_darwin.go`：创建 TUN/utun fd 后调用 `unix.CloseOnExec(fd)`，禁止子进程继承。
- 清理 qg 上遗留的 `qg_leg_stream_watch.sh`（持有继承 fd 的 watcher）。
- qg 使用 OS SSH（端口 2222）重启，避免 `rc-service stop` 断开内置 SSH 导致启动命令未执行。

## 验证

```
# 修复后 qg
ip -br addr | grep tun        -> 仅 tun0
ip route get 100.0.0.3        -> 100.0.0.3 dev tun0 src 100.0.0.2
nslookup git.migu.cn          -> 100.135.0.11
# watchdog/worker tun fd 数量 -> 0 / 1
```

## 部署

- Linux/macOS：qg/gg/jf/ms9/ms10/mb = `v0.8.1-94-gb20b1c4`
- Windows：Wintun 不经过该 fd 路径，保持 `v0.8.1-91-gddc2bb0`
