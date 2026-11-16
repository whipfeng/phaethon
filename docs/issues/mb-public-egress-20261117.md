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
