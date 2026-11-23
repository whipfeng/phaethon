# QG 旁路网关反复失效：UDP fd 泄漏（2026-11-20）

## 现象

- QG 控制台显示 TUN 不可用；`ip addr` 没有 `tun0`；admin 443/39999 TLS 握手超时；
- 进程仍在运行（`rc-service phaethon status` = started），但日志大量：
  `dial udp ... socket: too many open files`、`[V2-LOOKUP] no peer`、
  `[V2-ROUTE] lookup failed`、`linkNIC is nil`；
- `rc-service` 重启后 TUN 恢复，过一段时间再次失效（历史反复出现）。

## 现场证据（重启前）

| 项 | 值 |
|---|---|
| `/proc/<pid>/limits` Max open files | 4095 |
| `/proc/<pid>/fd` 数量 | **4095 / 4095** |
| UDP socket（`ss -uanp`） | **4052 个**，全部 `UNCONN *%eth1:ephemeral`、无 peer |
| TCP socket | 17 LISTEN / **12 ESTAB** |

4052 个 UDP 没有对应的 4052 条 TCP 控制连接 → 不是“有人 idle 在用 SOCKS5 UDP”，
而是 **UDP socket 生命周期结束后没有被关闭**。

## 根因

**为什么 TCP 控制面不在了、UDP 还在？** 因为这些泄漏的 UDP 不是 SOCKS5 关联，本来就没有 TCP
控制面。它们的 owner 是 gVisor 的 UDP forwarder endpoint，形成的是“端点等 handler、handler
等端点”的循环：

1. `mesh/netstack.go` 的 `acceptUDP` 收到 LAN 的 UDP 首包后创建 `udp.ForwarderRequest` 端点
   `ep` 和 `gonet.UDPConn`：
   ```go
   ep, _ := r.CreateEndpoint(&wq)
   defer ep.Close()
   conn := gonet.NewUDPConn(&wq, ep)
   defer conn.Close()
   HandleUDPConn(conn, ...)   // 同步调用
   ```
2. `HandleUDPConn` → `relayUDP` 同步阻塞在 `conn.Read` / `targetConn.ReadFrom`；
3. UDP 没有 FIN/close 事件。客户端停止发包后，`relayUDP` 永远等不到数据，也等不到关闭；
4. 于是 `HandleUDPConn` 永远不返回，`acceptUDP` 里的 `defer ep.Close()` / `defer conn.Close()`
   永远不执行；
5. `relayUDP` 也不会执行 `targetConn.Close()`（在 `HandleUDPConn` 的 defer 里）。

结果：gVisor endpoint 和 handler 创建的 OS UDP socket 互相等待，永久占着 fd。现场
4052 个无连接 UDP、只有 12 条无关 TCP ESTAB，完全符合这条路径。

**SOCKS5 UDP ASSOCIATE** 是另一条路径：它有 TCP 控制连接，正常应由控制连接 EOF 触发
`relay.Close()`；但控制连接还活着、长期无 UDP 时也没有空闲超时，属于次要泄漏点。

**fd 上限 4095 偏低**：泄漏累积到上限后，TUN 创建/重建、DNS、admin TLS 全部失败，
表现为“TUN 不可用/旁路网关失效”，并非进程崩溃。

## 修复

- `relayUDP`：增加 5 分钟空闲 watchdog（每 30s 检查），空闲后关闭 netstack 连接和 targetConn。
  这打破了第 2–5 步的循环：`relayUDP` 返回 → `HandleUDPConn` 返回 → `acceptUDP` 的 defer
  关闭 endpoint 和 conn → OS UDP socket 释放。这是补上 UDP 缺失的生命周期，不是只关 socket。
- SOCKS5 UDP relay：增加 5 分钟空闲 watchdog；client 数据报、下游回包、early reply
  都刷新活跃时间；空闲后关闭 UDP socket 和 TCP 控制连接。
- 日志：`tun` 模块默认级别提升为 warn；TUN interface disappeared、重建失败、
  `TUN initialization failed` 改为 error，避免被全局 error 过滤。
- 建议 QG OpenRC 启动前 `ulimit -n 65535` 作为兜底（运维项）。

## 二阶故障：健康检查把 “fd 耗尽” 误判为 “TUN 接口消失”

上面解释了 fd 为什么会被耗尽，但还差一环：**fd 耗尽只会让新的 TUN 建不起来，不应把已经在用的
TUN 弄没**。追代码后确认这是 `tunHealthLoop` + `InterfaceExists` 的误判，属于代码自身缺陷，
不依赖任何外部操作：

1. `tunHealthLoop` 每 5s 调 `InterfaceExists(name)` 判断接口是否还在；
2. Linux 旧实现用 `net.Interfaces()` 枚举，失败时**直接返回 false**；
3. `net.Interfaces()` 内部要创建 netlink socket。进程 fd 打满（现场 4095/4095）时
   `socket(AF_NETLINK)` 返回 `EMFILE`，枚举失败 → 返回 false；
4. 健康循环把 false 当作“接口被删了”，调用 `StopTUN()` **主动关闭 TUN fd**（此时接口才真的消失），
   随后 `StartTUN()` 又因 EMFILE 失败 → 节点停在 mesh-only，控制台显示 TUN 不可用。

也就是说：**UDP 泄漏是弹药，健康检查的 `err != nil → false` 是扳机**。只要 fd 压力导致枚举
失败一次，健康的 TUN 就会被健康循环自己拆掉，所以“没人动过也丢 TUN”完全成立。

同类代码缺陷一并修正：

| 缺陷 | 后果 | 修复 |
|---|---|---|
| `InterfaceExists` 把枚举失败当“不存在”（Linux/Darwin/Windows） | 压力下误删健康 TUN | 改为 `(bool, error)`；错误不再等于不存在 |
| Linux 枚举依赖 netlink socket | fd 打满时必然枚举失败 | 主路径改用 `stat("/sys/class/net/<name>")`（不占 fd），枚举仅作 sysfs 缺失时兜底 |
| Linux TUN fd 以阻塞方式打开 | 空闲时 `Close` 无法唤醒阻塞中的 `read(2)`，`StopTUN` 的 `tunWG.Wait()` 可永久挂起（admin 关 TUN 卡死） | 改用 `O_NONBLOCK` + `poll(200ms)`，读写期间用 RWMutex 保护 fd 不被复用；`Close` 置位后读写返回 `ErrSessionClosed` |
| `readLoop` 非预期退出后 `tunRunning` 仍为 true | 状态显示正常但不再读包，健康循环永远不重建 | 新增 `readLoopDead` 标记，健康循环据此重建 |
| `readLoop` 持续读错误时无退避 | 坏 fd 上空转刷日志 | 连续错误 50ms 退避，20 次后退出交给健康循环 |

### 为什么频繁重启 TUN 时旁路网关还能工作

临界期健康循环会成对执行 `StopTUN`/`StartTUN`，但 LAN 只感到短暂卡顿而不是断网：

- `StopTUN` 只停主机侧 TUN、路由和系统 DNS，**gVisor netstack / mesh / P2P / DNS hijacker
  都不重启**，已建立的 TCP 流和 SNAT 状态保留；
- `StartTUN` 秒级重建（Linux 重新申请同一个 tun0、重装 /29 地址与排除路由），丢包由 TCP 重传覆盖；
- 健康检查周期 5s，最坏每 5s 抽一下；
- 更关键的是它形成了“泄压阀”：`StopTUN` 释放 TUN fd 和 exec 临时 fd，紧接着的 `StartTUN`
  又能挤进 fd 限额，于是“拆掉→重建→恢复”成立，泄漏被掩盖；直到 `StartTUN` 需要的一组
  fd 都凑不齐才永久失败。修复后枚举失败不再触发 StopTUN，这类无谓抖动消失。

## 验证

- 单测/构建：`go build ./...`、`go test ./server ./handler` 通过；
- 部署后观察：
  - `ss -uanp | grep phaethon | wc -l` 不再持续增长；
  - `/proc/<pid>/fd` 稳定，不再逼近 4095；
  - `ip -br addr` 持续存在 `tun0`，`/api/tun` 保持 `available=true,bypassGateway=true`；
  - 空闲 5 分钟后对应 UDP flow 被回收（可用日志
    `UDP flow idle for 5m0s, closing` / `UDP ASSOCIATE idle for 5m0s, closing` 确认）。

## 部署记录（2026-11-20）

- 先灰度 QG：`v0.8.3-32-ge0303e2` upload+activate 成功；
- 随后全量扩散：9 节点全部 `v0.8.3-34-g312a646`、`status=succeeded`；
- QG 现状：`tun0` 在、fd 143、UDP socket 0，`/api/tun` 正常；拓扑 9 节点/22 条边；
- 待观察 fd/UDP 不随空闲流量增长（重点看 `ss -uanp | grep phaethon | wc -l`）；
- 建议 QG 的 OpenRC 启动前加 `ulimit -n 65535` 作为兜底（当前进程仍是 4095 软限制）。

## 部署记录：TUN 健康检查修复（2026-11-20 13:00）

- 版本：`v0.8.3-38-gff94ac8`（含二阶修复；`build-pkg.sh` 交叉编译显式传 `-version`）；
- 通过控制台 upload+activate 自替换：vm/qg/jf/mb/ms9/ms10/qgw/deskzq，全部
  `/api/packages/pin` = `current=v0.8.3-38-gff94ac8, status=succeeded`；
- gg：从 VM 到 `gg.phn:39998` 的 admin TLS 握手失败（重启后依旧，属于既有路径问题），
  改走 OS SSH（`layer4@loc.gg.phn`）直接替换稳定路径二进制，`--version` 与
  `P2PManager initialized` 均为 `v0.8.3-38-gff94ac8`；
- deskw：不在当前 mesh 拓扑（9 节点不含 deskw），本次跳过；
- 部署后拓扑 9 节点 / 22 条边完整（vm↔gg、gg↔jf/mb/ms9/ms10 均在）；
- QG 现状：`/api/tun` 为 `available=true, enabled=true, running=true, degraded=false`,
  `tun0` 在读包（重启后 `readPackets≈12 万`）；worker fd 67、watchdog fd 140、
  UDP socket 10；`logs` 仅 `gVisor netstack started` / `TUN device started on tun0`，
  无重启循环、无 `interface check failed`。


## 部署记录：deskw 补部署引导（2026-11-23）

v0.8.3-38 全量扩散时 deskw 离线被跳过；11-23 08:01 deskw 上线后补部署到
`v0.8.3-39-g74035b2`（含 14aa3d1 TUN 日志级别修复）。**deskw 原为 v0.8.0-653
（< 664，watchdog 不识别 upgrade-request），不能走 upload+activate，按 5.2 引导
部署改为稳定路径直接替换**：

1. 证书本地化：`client-all.crt/key` 从 QG `/root/` 取回本地 `~/.phaethon-certs/`
   （0600，仓库外），本地 mesh DNS 可直连 `deskw.phn` 的 admin/内置 SSH；
2. 构建签名包并 upload（进 pending，不影响运行版本），包/解压后二进制哈希与
   本地构建一致（`79de6581...9039`）；
3. 内置 SSH（本机 id_rsa 已注册为 `VM-rsa-cmll`，免加临时钥匙）执行暂存：
   `phaethon.exe`(653) → rename `phaethon.old.exe`（运行中 exe 可改名不可覆盖），
   新二进制复制到稳定路径；
4. 重启按 11-19 纪律走"验证过的一次性计划任务"：写 `start-phaethon.vbs`
   （**显式设 `CurrentDirectory`**——`dataDir` 取自 `os.Getwd()`，计划任务默认
   System32 cwd 会导致新建空库丢配置），先用无害写文件任务验证 SSH 断开后
   仍会执行，再注册 08:45 重启任务、08:44:03 杀进程；
5. 实际生效的是**用户 08:47:42 手动双击重启**：08:45 任务实例与手动启动发生
   实例锁冲突（用户看到的报错即此，无害）；最终状态正确。

**验收**：`/api/packages/pin` = `v0.8.3-39-g74035b2, status=idle, lastError=""`；
watchdog+worker 两进程均在稳定路径（新架构"worker 恒用稳定路径"生效）；主日志
`P2PManager initialized (version=v0.8.3-39-g74035b2)`，reverse-id deskw 加载正常；
mesh 身份不变（100.159.0.10），deskw↔jf 边双向恢复；内置 SSH/mTLS admin 正常。
清理：计划任务×2 删除、pending 包删除、暂存目录删除、无 System32 残留 data；
回滚路径 `phaethon.old.exe`(653) 在位。

**经验**：① `.phn` 域名经 macOS 系统 resolver 有负缓存，nslookup 能解析但
curl 报 exit 6，用 `curl --resolve deskw.phn:443:100.159.0.10` 绕过；② Windows
节点从计划任务/wscript 启动必须在 vbs 里显式 `CurrentDirectory`，否则 data
目录会落到 cwd 下；③ 运行中的 exe 可以 rename 但不能 overwrite，稳定路径
替换用 rename+copy 即可在线暂存，零停机窗口留给最后重启。
