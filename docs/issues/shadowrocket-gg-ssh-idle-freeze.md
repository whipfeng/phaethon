# Shadowrocket → GG → QG/VM SSH 空闲后卡死（freeze）问题

**状态**: 调查中（2026-11-08 首次抓包取证完成，根因初步定位但未实锤）
**严重级别**: 中（影响手机端 SSH 体验，需手动重连）
**首次发现**: 2026-11-07
**关联**:
- 链路 SR → GG:39988（SNI trojan `www.dynamictj.com`）→ mesh → VM/QG sshd
- SNI keepalive 修复 commit 542ebdf（已合并未部署）
- [[mesh-data-plane-stall]]（VM↔QG mesh 内部 stall 是同一类问题的另一面）

## 症状

iPhone Shadowrocket 经 GG trojan 入向访问 mesh 内 sshd（`ssh loc.vm.phn` 或 `ssh qg.phn`），一段时间无交互后：
- 终端冻结（敲键无回显、不显示断开）
- 偶尔显示断开
- 重新连接后发现之前输入其实到达 sshd 但无回显（单向通）
- 重连后新会话正常

## 抓包取证（2026-11-08）

### 抓包配置
- 位置：GG 上 tcpdump，ring buffer 4×30MB
- 过滤：`host 183.212.182.203 and port 39988`（SR 出口 + GG SNI trojan 端口）
- 时长：10:56 → 14:13（3 小时 17 分钟，43887 个包）
- pcap 位置：`QG:/tmp/sr.pcap0`（GG 原始文件已 kill 抓包进程后保留）

### 关键发现

**Freeze 时刻：13:55:59 → 14:08:11（11 分钟静默）**

freeze 前最后几个包（连接 `SR:2607 ↔ GG:39988`）：

```
13:55:59.685282  In  SR.2607 → GG.39988: [P.] seq 30630:30654 ack 2608315  (手机最后发 24B 数据)
13:55:59.685286  Out GG.39988 → SR.2607: [.]  ack 30654                       (GG 回 ACK)
13:55:59.685389  Out GG.39988 → SR.2607: [P.] seq 2608519:2608543 ack 30654   (GG 回 24B 数据)
13:55:59.685425  Out GG.39988 → SR.2607: [F.] seq 2608543                     (GG 发 FIN ← 主动关闭)
13:55:59.685734  In  SR.2607 → GG.39988: [F.] seq 30654 ack 2608315            (手机也回 FIN)
13:55:59.685741  Out GG.39988 → SR.2607: [.]  ack 30655                       (GG ACK 手机 FIN)
13:55:59.756245  Out GG.39988 → SR.2607: [F.] seq 2608543 ack 30655            (GG 重传 FIN)
13:55:59.996249  Out GG.39988 → SR.2607: [FP.] seq 2608315:2608543 ack 30655   (GG 重传 FIN+PSH+228B 数据)
13:56:00.468248  Out GG.39988 → SR.2607: [FP.] seq 2608315:2608543 ack 30655   (重传 1, +0.5s)
13:56:01.404251  Out ... [FP.] 同样 seq ...                                     (重传 2, +0.9s)
13:56:03.324250  ... (重传 3, +1.9s)
13:56:07.100248  ... (重传 4, +3.7s)
13:56:14.908253  ... (重传 5, +7.8s)
13:56:30.268250  ... (重传 6, +15.4s)
13:57:00.476253  ... (重传 7, +30.2s)
────────── 之后 GG 完全停止重传，进入 11 分钟静默 ──────────
14:08:11.388729  In  SR.2763 → GG.39988: [S] seq 3085471425                   (SR 发起新连接，新源端口)
14:08:11.388756  Out GG.39988 → SR.2763: [S.] ...
... 新连接健康通信约 10 秒 ...
14:08:21.675247  Out GG.39988 → SR.2763: [F.] seq 951 ack 1151                (GG 又主动 FIN)
14:08:21.939251  Out ... [FP.] seq 927:951 ... (GG 重传 FIN+PSH+24B)
... 同样的重传循环 ...
14:09:30.555252  ... (最后一次重传)
────────── 又一段静默 ──────────
14:11:27.878088  In  SR.2986 → GG.39988: [S] seq 1435248109                   (SR 又发起新连接)
... 同样模式 ...
```

### Freeze 根因（初步）

1. **不是 NAT 表项过期**——之前猜测是"NAT 空闲超时导致下行黑洞"，抓包实锤**不是**
2. **是 phaethon 主动 FIN**：GG（phaethon）主动向 SR 发 FIN，但 FIN+PSH 重传包**永远收不到 ACK**
3. **FIN 后运营商 NAT 丢包**：一旦连接进入半关状态（FIN 已发），运营商 CGNAT 把后续包全丢，但 ESTABLISHED 状态期间包正常通过
4. **SR 不知道连接已关**：因为 FIN 送不达，SR 客户端认为还连着 → 用户感知"卡死"
5. **SR 重连机制**：SR 在用户下次操作时（~11 分钟后）才发起新连接

### 待实锤问题

**Q4: 为什么 phaethon 主动 FIN SR 这条 TLS 连接？**

可能性：
- (a) sshd 端关闭 → mesh 链路传递关闭 → trojan 关 TLS
- (b) mesh 链路故障（gVisor endpoint invalid）触发 phaethon 关闭入向
- (c) phaethon 内部 watchdog/timeout 触发关闭
- (d) 其他

**当前障碍**：freeze 时段（11/8 13:55）GG 上 `phaethon.log` **完全无日志**（最后写入 11/8 01:41:46，12 小时静默）。无法定位 phaethon 当时为什么关连接。

### 旁证：gVisor endpoint invalid 错误（11/7 23:49 ~ 11/8 01:41）

GG 日志末尾大量同类错误：

```
[TCP-SEND] failed: local=100.179.0.10:443 remote=100.212.0.3:28861 
state=ESTABLISHED flags= PA payload=24 
err=*tcpip.ErrInvalidEndpointState endpoint is in invalid state
```

- `100.179.0.10` = GG 自己 mesh VIP
- `100.212.0.3` = VM 的 mesh VIP
- `payload=24` = 每 2 分钟一次心跳
- 错误反复出现 = mesh 内部 TCP endpoint 已死，但应用层还在写

**推测**：mesh 内部 endpoint 死 → SSH over mesh 链路不通 → phaethon 关 SR 入向 TLS。**但无日志实锤 phaethon 关闭动作**。

## 下一步行动

1. **打开 GG phaethon 日志级别到 INFO/DEBUG**（当前静默异常，得先查清为什么 12 小时无日志）
2. **再抓一次 freeze 实锤**：日志打开后让 freeze 重现，看 phaethon 在 FIN 那一刻记录什么
3. **核查 worker 进程活性**：当前 worker PID 1496201（Nov07 起）跑了 12 小时无日志，可能已死但 watchdog 没察觉
4. **验证 SNI keepalive 修复部署后是否改善**：commit 542ebdf（已合并未部署）让 GG 入向补 `configureTCPConn`（keepalive period 30s），即使 mesh 内部挂了，入向也能在分钟级 RST 通知 SR——这样至少 SR 不会"以为还连着"

## 临时缓解

用户侧：
- SSH 客户端 `ServerAliveInterval 30 ServerAliveCountMax 3`（应用层心跳，NAT 不会当空闲）
- 用 tmux/screen 兜底，重连后 shell 状态还在
- 部署 542ebdf 后 freeze 时 GG 会在分钟级 RST，SR 能立刻知道断开（而非卡死）

## 时间线

| 日期 | 事件 |
|---|---|
| 2026-11-07 | 首次记录问题，初步分析（错判为 NAT 表项过期） |
| 2026-11-07 | commit 542ebdf SNI trojan 入向补 keepalive（已合并未部署） |
| 2026-11-08 10:56 | GG 上启动 tcpdump 抓包 |
| 2026-11-08 13:55 | Freeze 重现（连接 :2607）|
| 2026-11-08 14:08 | SR 重连（连接 :2763） |
| 2026-11-08 14:11 | SR 又重连（连接 :2986） |
| 2026-11-08 14:13 | 抓包停止，定位到 freeze 主因：phaethon 主动 FIN + NAT 丢 FIN 后包 |
| 2026-11-08 | 发现 GG 日志 12 小时静默异常；待开日志后再抓一次实锤 phaethon 关闭原因 |
| 2026-11-08 | 加 relay 日志（util/util.go）；升级 GG 二进制时触发版本号 bug 事故；事故已复盘并修复；GG 已就位带 relay 日志等 freeze 复现 |

---

## 2026-11-08 二次取证 + 升级事故复盘（留跑期交接）

### Freeze 二次抓包（连接 :2919，freeze 时刻 15:11:49.728）

GG 上 tcpdump 抓包继续，第二次 freeze 抓到同样的模式：

```
15:11:49.728  Out GG.39988 → SR.2919: [F.]  ← phaethon 主动 FIN
... FIN+PSH 重传序列，SR 不 ACK ...
```

### Q4 根因初步定位

FIN 来自 `server/trojan.go:158 util.RelayWithRateLimit` → `util.Relay` 的 io.Copy 关闭路径：

```go
// util.Relay 内 io.Copy 返回后：
n, err := io.Copy(dst, src)
// 原本零日志，直接 CloseWriteOrClose(dst) 或 left.Close()/right.Close()
```

**根因方向**（待下次 freeze 实锤具体方向和 err）：
- io.Copy 任一侧 EOF/err → CloseWriteOrClose(dst) → TCP FIN
- 日志原本静默，无法看到 EOF 来自 SR→GG 上行还是 mesh→GG 下行
- 本次已加 debug 日志记录方向 + bytes + err

### 已加日志（util/util.go）

`Relay` 和 `RelayWithRateLimit` 的 copy 函数新增：

```go
copy := func(dir string, dst, src net.Conn) {
    defer wg.Done()
    n, err := io.Copy(dst, src)
    if err == nil {
        LogModuleDebug("relay", "[%s<->%s] %s io.Copy done bytes=%d (EOF), half-closing dst", ...)
        CloseWriteOrClose(dst)
    } else {
        LogModuleInfo("relay", "[%s<->%s] %s io.Copy fail bytes=%d err=%v, closing both", ...)
        left.Close(); right.Close()
    }
}
```

下次 freeze 在 GG 日志 grep `relay` 能看到具体方向（`up`/`down`）+ err（EOF / read timeout / endpoint invalid / 其他）。

### GG 升级事故复盘

**事故**：部署带 relay 日志的二进制 `v0.8.0-355-g6b95636-relay-log` 后 GG worker 起不来，20s 超时回滚；二次升级又失败。用户手动恢复。

**根因**：版本号自定义后缀 `-relay-log` 触发 `p2p.CompareVersions` 的 `commitPattern = -\d+-g[0-9a-f]+$` 锚定 `$` bug：

- 自定义后缀导致 hash 后接非 `$` 结尾 → 正则完全不匹配 → self 版本被判 untagged（`tag=nil`）
- pkg 里的 `v0.7.3-64-g4c45a93-dirty` 反而 tag 非 nil
- `selectWorkerBinary` 走"tagged > untagged"分支 → 选了旧 pkg v0.7.3
- v0.7.3 与当前 db schema 不兼容 → worker 起不来 → 回滚

### 已修复

1. **p2p/p2p.go**：`commitPattern` 去 `$` 锚定 + `FindStringSubmatchIndex` 切 match 起点，允许 hash 后接自定义后缀
2. **main.go selectWorkerBinary**：加 `selfUnparseable()` 防御——self 版本无法解析时（dev/自定义后缀）忽略 pkg 直接用 self
3. **p2p/version_test.go**：加事故回归测试 `v0.8.0-355-g6b95636-relay-log` vs `v0.7.3-64-g4c45a93-dirty`

### 新增自省规则（memory）

`feedback-version-stamp-no-custom-suffix.md`：构建版本号严格用 `git describe --tags --always --dirty` 原文，**禁止加任何自定义后缀**。想标记特殊改动写到 commit message / tag / 分支名。

### 当前 GG 状态（freeze 排查留跑期）

- 版本：`v0.8.0-355-g6b95636-relay-log`（带 relay 日志）
- 模式：watchdog + worker（PID 3892471 + 3892577）
- packages 目录：`packages.disabled-relay-log`（清空避免 watchdog 选旧 pkg）
- 日志级别：global=info + relay/trojan/server/handler/base/control=debug
- tcpdump 抓包进程 PID 3895083
- SR 已重连（:2798）

### VM 状态

- 日志级别已通过 admin API PUT info/debug
- **未部署**带 relay 日志的新二进制（`Desktop/Workspace/phaethon/phaethon.exe.new` 已构建）

### 待办（留跑期）

1. **等 freeze 复现**：用户切 app 让 SR idle，freeze 后在 GG 日志 grep `relay` 看具体方向 + err
2. **VM 部署带 relay 日志的二进制**：`phaethon.exe.new` 已构建，等一条命令内 stop+replace+start（参考 GG 脚本 `tmp_gg_upgrade_relay_log.sh`）
3. **部署完成后恢复 GG packages 目录**：把 `packages.disabled-relay-log` 改回 `packages`（在新 .pkg 替换旧 v0.7.3 之后）
4. **commit 当前所有改动**：p2p/main/util/server/admin 改动 + 新测试 + memory（用户授权后）

### 巡检（已建立 cron 任务 ID 960a0b8a）

每 10 分钟巡检：
- VM：进程/admin 活性、dump 检查、重启原因
- GG：进程与版本、39988 监听、mesh gossip、抓包活性

任务 7 天自动过期（约 2026-11-15），过期后按 `memory/reference-vm-health-cron.md` 重建。

---

## 2026-11-08 18:28 第三次取证：冻结点是「下行单向失效」

GG 已重新部署正式版 `v0.8.0-359-g7b39447`（含版本比较修复 e61ab02 + relay 诊断日志 d509cc3），
日志级别恢复 `global=info` + relay/trojan/server/handler/base/control=debug，tcpdump 环形抓包常驻。

### 连接生命周期

同一连接（GG 侧 `192.168.16.2:39988 ↔ 183.212.182.203:2909`，mesh 侧 `100.179.0.3:56560 ↔ 100.212.0.73:22`）：

| 时间 | 事件 |
|---|---|
| 18:12:13 | VM phaethon `handleConn START`，`loc.vm.phn:22 → DIRECT` 到本机 sshd |
| 18:12 ~ 18:28 | 下行累计 2,018,093 字节，上行 12,837 字节；末段空闲 |
| 18:28:49.759959 | **In SR → GG `[P.] len=24`**（手机最后一批输入，成功送达） |
| 18:28:49.760080 | Out GG → SR `[P.] len=24`（回显/响应，序列 2228399:2228423） |
| 18:28:49.760108 | **Out GG → SR `[F.] seq=2228423`**（GG 发 FIN） |
| 18:28:49.765977 | In SR → GG `[F.] seq=19414 ack=2228399`（SR 回 FIN，**但未确认那 24 字节**） |
| 18:28:50 ~ 18:29:48 | GG `[FP.] len=24` 重传 8 次（0.05/0.46/0.94/1.86/3.65/7.76/15.4/29.2s 退避），**全部无 ACK** |
| 18:28:49 | GG relay：`left->right io.Copy done bytes=12837 (EOF)` / `right->left io.Copy done bytes=2018093 (EOF)` |
| 18:28:49 | VM phaethon：`handleConn END` + `HANDLECONN-RETURN` |
| 18:29:48 后 | 完全静默，SR 侧始终不知连接已关 |
| 18:31:35 | 用户重连（GG 侧新连接 :29129 → VM 侧 :61864） |

### 直接机制：下行单向失效

| 方向 | 状态 | 证据 |
|---|---|---|
| 上行 SR→GG→VM sshd | **通** | 24 字节成功送达 GG 并被转发处理 |
| 下行 GG→SR | **断** | 24 字节 + FIN，SR 全部不 ACK |

这与用户症状「敲键能到 sshd、但回显回不来、也不显示断开」完全吻合，也解释了「重连后发现之前的输入其实到达了」。
注意：不是整条链路失效——SR 的 FIN 能到达 GG，说明上行映射仍在；失效的是 **SR 侧不再接收入向数据**。

### 本次排除项（均带证据）

| 假设 | 结论 | 证据 |
|---|---|---|
| mesh 链路断 / 抖动 | ❌ 排除 | GG 侧 18:28:47/48/52/55 均 `RECOMPUTE-ROUTES peerCount=3 peers=[vm jf mb]`，路由 11 条稳定 |
| VM sshd 空闲超时主动关 | ❌ 基本排除 | `C:\ProgramData\ssh\sshd_config` 中 `ClientAliveInterval` 被注释（默认 0 = 不超时）；OpenSSH Operational 日志 18:20–18:32 仅一条 18:31:37 新连接 Accepted，**无任何 Disconnect 事件** |
| phaethon 强杀连接 | ❌ 排除 | relay 两方向都是 `EOF`（正常关闭），非 `err`；对比 15:52 那次才是 `operation aborted` |
| CGNAT 整条映射过期 | ❌ 排除 | SR 的 FIN 仍能到达 GG |
| 应用层无数据导致看门狗 | ❌ 排除 | 数据面已无空闲杀（timeout-audit A1/A2/A3） |

### 仍未实锤：谁先发起关闭

GG 侧只能看到「`right`（GG→VM 的 mesh 连接）先 EOF，随后 `left`（SR 的 TLS）也被关闭」。
而 GG 的 `right` 对端是 VM phaethon 的 `left`，其 EOF 可能来自 VM 侧 sshd，也可能来自 mesh 层。
**这一步不闭环，不能判定 phaethon 无责。**

需 VM 侧 relay 日志区分：

| VM relay 先看到 | 结论 |
|---|---|
| `right`（本机 sshd）先 EOF | 源头在 sshd / Windows 侧，phaethon 仅正常传播关闭 |
| `left`（来自 GG 的 mesh）先 EOF | 源头在 GG 侧，需继续深挖 phaethon 逻辑 |

### 24 字节的性质：TLS close_notify（已核实，非缺陷）

初判曾怀疑 `util/util.go:60` 的 `CloseWriteOrClose(dst)` 对 `*tls.Conn` 会退化为 `Close()` 全关，
**该怀疑已证伪**：

- `CloseWriteOrClose` 用 `interface{ CloseWrite() error }` 做类型断言（`util/util.go:36-42`）
- `go doc crypto/tls.Conn` 确认 **`func (c *Conn) CloseWrite() error` 存在**（Go 1.26.2）
- TLS 1.3 加密后的 close_notify alert 尺寸 = 5（record header）+ 1（inner type）+ 2（alert）+ 16（AEAD tag）= **24 字节**

因此 GG 发的那 24 字节正是**标准 TLS 关闭通知**，随后才是 TCP FIN。
`Relay` 的关闭顺序（一侧 EOF → `CloseWrite(dst)` → 发 close_notify → 调用方 defer Close → FIN）**符合 TLS 规范**。

结论：**这条路径上没有发现 phaethon 侧的协议缺陷**。真正的问题是这 24 字节 close_notify 在 GG→SR 方向始终未被 ACK，
以及其后的所有重传同样无响应。

### 历史同类事件（同一模式）

- 15:52:55 `SR:2766`：`right->left operation aborted`（mesh endpoint 被中止，性质不同，属异常中止）
- 18:11:16 `SR:2757`：GG 发 `[FP.] seq 1:25 ack 1 len=24`，同样 24 字节 + FIN 重传 8 次
- 18:13:26 `SR:2824`：完整四次挥手（Out FIN → In FIN → Out FIN）
- 18:28:49 `SR:2909`：本次，`[FP.] len=24` 重传 8 次

「24 字节 + FIN + 指数退避重传 ~60s」在三次事件中重复出现，是稳定特征。

### 下一步

1. ~~VM 部署带 relay 日志的版本~~ **已完成（2026-11-08 18:50）**：VM 部署 `v0.8.0-360-g3d0b468`，
   relay/trojan/server/handler/base/control 已置 debug，回滚包 `phaethon.prev-20261108.exe`。
   下次 freeze 时两端 relay 日志同时落盘，即可判定关闭发起方。
2. 客户端缓解：SSH 客户端配置 `ServerAliveInterval 30`、`ServerAliveCountMax 3`，并用 tmux 兜底
3. 若确认下行长期单向失效，评估是否需要用「关闭前短暂等待 close_notify ACK」或应用层 RST 兜底
   （注意：RST 会丢失 SSE/长连接语义，需用户拍板，且可能违反数据面不干预原则）

### 复现后判读表

| 侧 | relay 日志首个 EOF 方向 | 结论 |
|---|---|---|
| VM | `right`（本机 sshd） | 源头在 sshd / Windows 侧，phaethon 无责 |
| VM | `left`（来自 GG 的 mesh） | 源头在 GG 侧，继续深挖 |
| GG | `left`（SR 的 TLS） | 源头在 SR / 运营商侧 |

GG 侧命令：`grep relay /home/layer4/phaethon-gg/data/logs/phaethon.log`
VM 侧命令：`Select-String -Path C:\Users\Docker\Desktop\Workspace\phaethon\data\logs\phaethon.log -Pattern '\[relay\]'`

---

## 2026-11-08 19:14:37 第四次取证：SR 先关闭

GG 侧（trojan 入向）：

```
19:14:37 left->right  io.Copy done bytes=11965  (EOF)   ← SR 侧先 EOF
19:14:37 right->left  io.Copy done bytes=2446105 (EOF)  ← 随后 mesh 侧 EOF
```

VM 侧：

```
19:14:38 [HANDLECONN-EXIT] [100.179.0.3:61503->100.212.0.73:22] handleConn END
```

比 GG 晚 1 秒，为被动响应。**本次源头指向 SR 客户端**。

历史对照：

| 时间 | 首 EOF 方向 | 性质 |
|---|---|---|
| 15:52 | right 先，`operation aborted` | mesh endpoint 异常中止 |
| 18:28 | left 先，双向 EOF | SR 先关 |
| 18:49 | right 先，`connection reset by peer` | VM 重启所致（操作副作用） |
| 19:14 | left 先，双向 EOF | SR 先关 |

## VM 侧 relay 日志为什么之前是空的（已修复）

VM 的 mesh 入向路径是 `handleConn → DIRECT → handler.relay()`，而 `handler.relay()` 是
`handler/connection_handler.go:530` **自实现的双向转发**，此前完全没有日志插桩；
只有 GG 的 trojan 入向走 `util.RelayWithRateLimit` 才有日志。

**修复**（commit `3e38c19`，仅插桩不改逻辑）：给 `handler.relay` 两个方向各加
方向 / 累计字节 / 错误类型日志，格式与 `util.Relay` 对齐，便于两端逐行对照。

已部署验证（VM `v0.8.0-362-g3e38c19`），实机输出示例：

```
[relay] [101.72.192.157:443|100.212.0.1:54342<->172.30.0.2:54343|101.72.192.157:443] left->right io.Copy done bytes=3685 (EOF), half-closing dst
[relay] [同上] right->left io.Copy done bytes=5193 (EOF), half-closing dst
```

## 下一次 freeze 的闭环判据

三端同时在场时按下表判定，**首个 EOF 出现的一侧即关闭发起方**：

| 侧 | 首个 EOF 方向 | 结论 |
|---|---|---|
| GG | `left->right` | SR 发起关闭（客户端侧） |
| GG | `right->left` | mesh 侧发起，需看 VM |
| VM | `left->right` | GG 侧把关闭传播下来（被动） |
| VM | `right->left` | 本机 sshd 发起（Windows 侧） |

