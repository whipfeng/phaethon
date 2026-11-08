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
