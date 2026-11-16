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
