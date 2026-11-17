# MB 公网出访失败与 tun 归属排查（2026-11-17）

## 现象

MB（macOS，192.168.1.88）无法访问部分公网地址，DNS 解析超时。

## 排查结论

1. **不是 phaethon 的 tun 造成的。** `lsof` 显示 7 个 utun（unit 1~7）全部由
   进程 `identitys`（PID 599，用户 cmll）持有；phaethon（PID 49920/49921）
   没有 utun fd。utun0~6 的 IPv6 默认路由属于该第三方程序，与 phaethon
   的启停无关。

2. **IPv4 出访被 MB 的网关 QG 阻断。** MB 的默认路由是
   `192.168.1.101`（QG，DHCP 静态绑定 `e0:b5:5f:f0:2a:f0 → 192.168.1.88`）。
   QG 上 `iptables -P FORWARD DROP`（Docker 的默认策略），且没有
   192.168.1.0/24 的 MASQUERADE 规则，因此 MB 的转发流量被丢弃。
   `route -n get 223.5.5.5` 确认走 en0→192.168.1.101；`traceroute` 只到
   第 1 跳，TCP/DNS 全部超时。

   临时修复（仅限 MB，未放开全域）：
   ```
   iptables -I FORWARD 1 -s 192.168.1.88 -j ACCEPT
   iptables -I FORWARD 2 -d 192.168.1.88 -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
   iptables -t nat -A POSTROUTING -s 192.168.1.88 -o eth1 -j MASQUERADE
   ```
   验证：`curl --interface en0 --resolve www.baidu.com:80:36.152.44.132` → 200；
   `nc 223.5.5.5 53` 成功。

3. **mesh DNS 曾在 QG 重启后停止。** QG 日志在 2026-11-15 17:27:06 出现
   `[DNS-FWD] listener stopped`，之后没有重新绑定；MB 的 resolver 是
   100.0.0.3，因此 DNS 全部超时。对 QG 再做一次完整重启后 DNS 恢复：
   MB `nslookup www.baidu.com` 返回 fakeip `100.0.0.250`，
   `curl https://www.baidu.com` → 200。说明 DNS hijacker 的停止/重启
   顺序有缺陷，需要在 worker 重启后确保重新绑定（后续代码修复）。

## 后续

- QG 的 FORWARD/NAT 规则是运行时添加，需纳入 QG 初始化/运维脚本并持久化
  （属于部署行为，不写入应用代码）。
- DNS hijacker 在 worker 重启后未重新绑定的问题需要单独修（应用行为）。
- `identitys` 持有 7 个 utun 及 IPv6 默认路由，与本应用无关；如需清理由该
  程序或其使用方处理。

## 应用侧修复（2026-11-17，d7dbe9a）

按“旁路网关功能应由应用自带”的原则，在 tun 引擎（Linux）中实现
bypass-gateway 的 host 侧转发/NAT：

- 新增 `PHAETHON_GW_FWD`：放行 LAN CIDR 及其 ESTABLISHED/RELATED 回程；
- 新增 `PHAETHON_GW_NAT`：`-s <LAN CIDR> -o <upstream> MASQUERADE`；
- 跳转插入 FORWARD/POSTROUTING 第 1 位，优先于 Docker 的 FORWARD DROP；
- 每 15s reconcile（Docker 重启后自动恢复），TUN 停止时清理链和跳转；
- 仅 Linux 实现，其它平台 no-op。

已部署 QG `v0.8.1-47-gd7dbe9a`，并删除之前手工添加的
192.168.1.88 规则。验证（仅应用规则）：

```
FORWARD -j PHAETHON_GW_FWD
PHAETHON_GW_FWD -s 192.168.1.0/24 -j ACCEPT
PHAETHON_GW_FWD -d 192.168.1.0/24 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
POSTROUTING -j PHAETHON_GW_NAT
PHAETHON_GW_NAT -s 192.168.1.0/24 -o eth1 -j MASQUERADE
```

MB 复测：`https://www.baidu.com` → **200**，直连公网 IP 可达。手工 NAT
不再需要。

剩余：DNS hijacker 在 worker 重启后的重绑需要单独验证/加固（本次 QG
重启后 DNS 正常，但 11-15 17:27 曾出现 listener stopped 后未重绑）；
utun 归属 identitys，不是本应用。

## 更正（2026-11-17）：不需要 MASQUERADE

复核确认：MB 与 QG 同一 LAN，上游路由器 192.168.1.1 会像普通 LAN 客户端
一样 NAT 192.168.1.88，因此 QG 不需要 POSTROUTING MASQUERADE。

- 已回退应用侧新增的 MASQUERADE/网关链（revert 4065f6f、7374d09）；
- 保留 `tun/route_linux.go` 现有的三条 FORWARD ACCEPT 元组；
- QG 自升级重启到 v0.8.1-51-g7374d09 后，元组自动恢复，无 MASQUERADE：
  MB `https://www.baidu.com` → 200，DNS fakeip 100.0.0.250 正常。

之前 11-15 17:27 的故障是**那次重启后 TUN 引擎/既有 FORWARD 规则/DNS
hijacker 没有起来**（同一时刻 DNS-FWD 也 stopped），不是功能缺失，也不是
需要手工 NAT。本次自升级重启未复现；后续应对“重启后 TUN 初始化”做启动
自检/告警（检查 tun0、三条 FORWARD 元组、DNS 监听是否就绪），而不是加
MASQUERADE。

## 最终修复（50fed3a，v0.8.1-53-g50fed3a）

- 撤回 MASQUERADE/新增链（revert 4065f6f、7374d09）；不引入新 NAT；
- 复用 `route_linux.go` 既有三条 FORWARD 元组，抽出幂等
  `ensureForwardRules`；
- TUN 启动后每 30s `gatewayHealthLoop` 重新置 `ip_forward=1` 并补回
  缺失的 FORWARD 元组；teardown 置位后停止；
- DNS hijacker 启动失败重试 3 次（1s 间隔），覆盖重启时端口/监听竞态。

验证：部署 v53 后手动删除 `eth1→eth1 ACCEPT`，约 50s 内被自动补回；
MB `https://www.baidu.com` → 200，DNS fakeip 100.0.0.250 正常，无
MASQUERADE。11-15 17:27 的完整根因未复现，但“重启后既有规则/DNS 未就绪”
这一类问题现在有自检/重试兜底。

## 事2验证：启动/交接分支日志（2026-11-17）

新增 system 级启动日志（提交 385a024、41e9d71、326fe43）：
`[STARTUP] startEngine`（tun/bypass/dhcp/mesh 配置）、TUN start begin/ok/
failed、DNS bind/start 结果、startEngine 返回句柄；watchdog 侧记录
`[HANDOFF] stopping worker / supervision transferred`。

QG 自升级到 v0.8.1-58-g326fe43 后，新 worker 的启动日志完整：

```
[STARTUP] startEngine: tunConfig=true tun.enabled=true bypass=true dhcp=true meshMgr=true
[STARTUP] TUN start begin
[STARTUP] TUN start ok running=true
[STARTUP] DNS bind ok
[STARTUP] DNS start ok
[STARTUP] startEngine returned: tunMgr=true netstack=true handler=true meshMgr=true
```

同时验证：删除 `eth1→eth1 ACCEPT` 后约 50s 被 gatewayHealthLoop 自动补回；
MB 出访 200、DNS 正常。11-15 17:27 的故障在 v58 上未复现；upgrade.log
中没有 17:27 记录（该次走的是更早的升级机制），当前 handoff 路径连续
多次自升级都完整初始化。后续若再出现，`[STARTUP]` 日志会直接指出卡在
TUN 还是 DNS，健康循环/重试作为兜底保留。

事2验证 OK，转入事1（原配置 TCP 载体下的瓶颈定位）。

## 2026-11-16 复盘：17:27 的根因仍未实锤，但不是“缺功能”，是半初始化启动路径

复核 QG 日志（`/root/data/logs/phaethon.log`，第 366525 行起）：

```
17:27:02  upgrade requested: v0.8.1-11-gcac6c68
17:27:03  （旧 worker 仍在运行：反向客户端注册等）
17:27:06  Received signal terminated, shutting down...
17:27:06  [DNS-FWD] listener stopped
17:27:06  （新 worker 启动：DB loaded / P2PManager initialized / Binding mapping）
17:27:06  started ok 1
17:27:06+ 只有 reverse/subscription 日志，没有任何 DNS bind/start、tun: 日志
```

旧 worker 在 17:27:06 收到 SIGTERM 后才停 DNS；新 worker 同一秒完成
P2P/mapping 初始化，但**没有重新绑定 DNS**。日志里 `engine` 模块当时被
全局 error 级别压住，无法直接看到是哪个调用失败，所以“17:27 的根因”
仍然只能算**强相关、未实锤**。

### 代码里未闭合的竞态/半初始化路径（本次已确认）

`main_engine.go:startEngine` 在 v58 之后虽然加了 3×1s DNS 重试，但仍存在：

1. `StartStack()` 失败 → `return nil,nil,nil`，`main` 不退出，继续加载
   proxies/mappings：worker 活着、mesh/P2P 正常，但没有 netstack/TUN/DNS；
2. `BindNetstack()` 失败 → 同样 `return nil,nil,nil` 继续；
3. `StartDNSHijacker()` 3 次都失败 → 只打 WARN 后 `return nil,nil,nil`
   继续；没有周期性重绑；
4. `StartTUN()` 失败 → 显式降级为 mesh-only 继续运行；
5. `gatewayHealthLoop` 是在 `StartTUN()` **成功之后**才启动的——正好
   StartTUN 失败时它不存在，所以连 FORWARD 规则的自愈也没有。

也就是说：升级/重启时只要任一必需组件瞬时失败（旧进程资源释放、
tun/端口/路由竞态），新 worker 会以“半初始化”状态长期运行，watchdog
认为 worker 健康、不会重启，直到人工再重启。这与 11-15 17:27 的现象
（DNS 停、MB 断网、mesh 仍在）完全吻合，但该次日志级别不足以逐调用实锤。

### 修复（fail-fast）

`startEngine` 改为返回 `(..., error)`：

- `StartStack` / `BindNetstack` / DNS 3 次重试仍失败 / `StartTUN`
  （TUN 已启用且平台可用）失败 → 记录 `[STARTUP] ... failed`，停止已启动
  资源并返回 error；
- `main` 收到 error 后 `os.Exit(1)`，由 watchdog 重试（带退避），
  不再保留半初始化 worker；
- mesh-only 仅保留给“TUN 未启用/平台不可用”的合法场景。

下一步：部署 QG 后做“边跑 MB DNS/HTTP 探针边连续自升级/重启”的压测，
验证失败时只会短暂重启并从完整初始化恢复，而不是永久半死。

## 2026-11-16 22:xx 实锤：非稳定路径 worker 存活 + 残留 tun0

按上述压测配方在 QG 连续重启，MB 出访从某次重启后**持续 000**（3 分钟
以上），QG 无法通过内置 SSH 访问，但 OS sshd 仍可登录。现场：

```
ps: /root/phaethon.failed --enable-debug-api   ← worker 跑在非稳定路径
    ./phaethon --enable-debug-api              ← 新 watchdog
ip: tun0 UP，只有 fe80:: link-local，无 IPv4
STARTUP: TUN failed: tunsetiff: device or resource busy
         每 10s 重试一次，始终 EBUSY
```

两个具体根因（这次有现场证据）：

1. **服务 stop 规则漏掉非稳定路径**。QG init 的
   `pkill -9 -f '^(\./|/root/)phaethon( |$)'` 不匹配
   `/root/phaethon.failed`（`phaethon` 后没有空格/行尾），所以
   `rc-service restart` 只杀了稳定路径的 watchdog/worker，`.failed`
   worker 继续存活并持有 tun0。新的稳定路径 worker 每次 TUNSETIFF 都
   EBUSY。
2. **半初始化/一次性初始化**。旧代码（v69）在 TUN 失败时静默降级
   mesh-only，worker 活着、watchdog 认为健康，不会重启；
   v70 的 fail-fast 会退出重试，但残留 tun0 不清理就变成每 10s 一次的
   崩溃循环（日志里连续 `engine init failed: TUN start ... EBUSY`）。
   另外升级 marker 残留（16:00 的 handoff）会让新 watchdog 在启动时
   把新二进制改名 `.failed`/回滚，进一步制造非稳定路径进程。

### 修复与验证

- 应用：`startEngine` fail-fast（v70，已提交）；`CleanupResidual` 启动时
  无条件删除残留 `tun0`（v72，已提交）；不再让 TUN 失败静默 mesh-only。
- 部署：清理 QG `data/state` 的 upgrade marker/status/request，重命名
  `.failed`/`.old2` 备份；把 init 的 stop 规则放宽为
  `^(\./|/root/)phaethon`（任意后缀）。
- 验证：
  - 停服务后手工 `ip tuntap add dev tun0 mode tun` 制造持久 tun0，
    启动 v72 → 清理残留、`TUN start ok`、`DNS start ok`；
  - 连续 4 次 `rc-service restart`：全部一次启动成功；
  - MB 每秒 HTTPS 探针全窗口仅 **1 次 000**（重启窗口），随后全部 200；
  - 修复前同样的重启会造成 3 分钟以上持续 000，且不会自愈。

结论：17:27 那次**具体是哪个调用先失败**仍受当时日志级别限制、未逐调用
实锤；但“非稳定 worker 路径存活 + 残留 tun0 + 一次性 TUN/DNS 初始化”
这条完整故障链已经实锤并可复现，且已修复/自愈。后续启动失败会以
`[STARTUP] ... failed` + watchdog 重启的形式暴露，不会再静默半死。

### 精确清理（v73）

之前 v72 的 `CleanupResidual` 为了救 QG 是**按名字无条件删 tun0**，不够
精确。v73 改为：

1. `StartTUN` 在 `CreateDevice()` 成功后写
   `/var/run/phaethon-tun.owner`（magic + iface + pid）；干净 `StopTUN`
   时删除该 marker；
2. `CleanupResidual` 只在 **marker 存在且 iface 匹配、且链路类型确为
   tun/tap** 时删除；类型不符只告警、不删；
3. 兼容没有 marker 的旧版本：仍按“链路 IPv4 == 本机 mesh host IP”删除，
   这是唯一性证明；
4. 不再按名字裸删 `tun0`，不会误删其它应用的 tun0。

已知边界：2026-11-15 那种“旧版本留下的、无 marker 且无 IPv4 的 tun0”
无法精确判定归属，不会自动删除，需要部署时一次性清理（本次 QG 已手工
清理）。之后的新版本都会带 marker，可自动精确清理。

v76 进一步收紧 marker：记录 `instance`（data dir）和接口 MAC。
- 另一个 phaethon 实例（不同 data dir）的 marker 不会被本实例删除；
- 崩溃后同名 tun0 被别的应用/tun 复用（MAC 不同）时不删除；
- 只有 instance+MAC+tun 类型全部匹配才删。

### 仍会导致“TUN 起不来”的残留/冲突场景

精确清理之后，以下情况仍会让节点停在 degraded mesh-only（不会硬重启，
mesh 继续可用），需要人工介入：

1. **旧版本/外部工具留下的持久 tun0**：无 marker、无本机 mesh IP
   （例如 `ip tuntap add dev tun0`），无法证明归属 → 不删；`TUNSETIFF`
   失败，后台重试无效，需 `ip link delete tun0`。
2. **同名 tun0 被其它程序占用**：dummy/bridge/其它 VPN 的 tun；类型或
   MAC/instance 不匹配 → 精确清理拒绝删除；启动失败，degraded。
3. **非标准路径 worker 未被服务 stop 杀掉**：例如 `/root/phaethon.failed`
   这类进程仍持有 tun0；只要 stop 规则用 `pkill -x phaethon`（按进程名
   精确匹配）就会漏掉。部署侧必须用前缀匹配
   `^(\./|/root/)phaethon`（QG 已改）。
4. **多个 phaethon 实例/多 data dir 争用 tun0**：marker 的 instance
   不匹配时不会互删，但同一主机只有一个 tun0 名字，后启动的实例会
   degraded，需要改成不同接口名或只保留一个实例。
5. **marker 写入失败**（/var/run 只读、权限异常）：当前运行不受影响，
   但崩溃后残留无法精确判定，需人工清理。
6. **Windows Wintun 适配器残留**：`deleteTUNAdapter` 失败（被占用/锁）
   时下次启动可能复用或失败；由 Windows 清理逻辑按适配器名处理，
   失败时 degraded + 重试。
7. **非残留类失败**：/dev/net/tun 缺失、CAP_NET_ADMIN 权限、内核模块
   未加载等，重试也不会成功；节点保持 degraded mesh-only，直到主机
   条件修复。

### mesh-only 策略（v74 调整）

- TUN 未启用、或平台不支持（`tun.Available()==false`）→ mesh-only 是
  合法设计；
- TUN 已启用且平台可用、但启动失败 → **默认保留 mesh**：worker 不退，
  P2P/mesh transit、netstack DNS、admin API 继续工作；同时后台
  `tunRetryLoop` 以 2s→4s→8s→…→30s 退避重试 `StartTUN`，恢复后打
  `[TUN] recovered`，LAN 出访自动恢复，不需要重启 worker；
- 降级状态可见：`/api/tun` 增加 `degraded` / `lastError`，日志首次失败和
  每 10 次重试告警；
- **不对 TUN 做硬重启**：持续硬重启没有意义——TUN 初始化在进程内重试即可
  复现同样的清理/创建流程，重启 worker 只会抖动所有 mesh 会话，且永久性
  故障（权限/设备被占用）重启一万次也不会好。只有 netstack / DNS 这类
  “mesh 本身无法工作”的核心组件失败才 fail-fast 交给 watchdog。

这样既不会因为 TUN 故障丢掉基础 mesh 能力，也不会回到 11-15 那种
“mesh 正常、LAN 静默断网且永不重试”的半死状态。

验证（v74，QG）：停服务后 `ip link add tun0 type dummy` 制造 TUN 启动
冲突，再启动：

```
ps:          worker 存活（PID 不变）
/api/tun:    running=false degraded=true lastError="tun: create device: ..."
/api/p2p:    vm/mb/jf 全部 upToDate
```

删除 dummy 后无需重启：45s 内 `/api/tun` 变为 `running=true degraded=false`，
tun0 恢复 `100.0.0.2/29`，MB `HTTP=200`、DNS fakeip 正常。说明 mesh 能力
在 TUN 故障期间保持，TUN 也能自动恢复。
