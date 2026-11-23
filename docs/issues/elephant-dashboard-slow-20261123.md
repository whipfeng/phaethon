# elephant 仪表盘经旁路网关访问奇慢（2026-11-23）

**创建时间**: 2026-11-23
**状态**: 已缓解（坏窗口自愈，当前全路径健康；成因未定论但可观测性已修复，复发可三步定位）
**优先级**: 中

## 问题描述

用户从本机（Mac，192.168.1.88，经 QG 旁路网关）访问
`https://10.193.131.25/elephant/default/#/dashboard`（deskw 办公网段内网应用）
"又奇慢无比"：单个 2MB JS 资源 20s+（~105KB/s），页面整体加载几十秒；
偶发连接建立阶段卡死（7s / 30s 超时）。

**流量路径**：Mac →(LAN/WiFi)→ QG 旁路网关(FORWARD→tun0) → gVisor netstack
→ RouteSelector Branch 3（deskw 通告的 10.193.131.0/24，IPIP 封装）
→ P2P：QG→jf（MGMS_HT h_tunnel）→ jf→deskw（DESK_TJ h_tunnel）→ deskw → 办公 LAN → elephant 服务。
即**两条 h_tunnel 腿串联**。

## 隔离矩阵（实测数据）

| # | 测试 | 结果 |
|---|---|---|
| 1 | deskw 本机 → elephant（纯 LAN） | 78-272ms，健康 |
| 2 | QG 发起 → elephant（同 mesh 路径） | TTFB 191-235ms 稳定；吞吐 **~1.0-1.2MB/s** |
| 3 | Mac ↔ QG 纯 LAN（scp 20MB，不经 TUN） | **~2.7MB/s** |
| 4 | **Mac → elephant（旁路+mesh，IP 直连）** | **38-150KB/s**，单流忽快忽慢（同文件 105KB/s→1.28MB/s），偶发 7s/30s 建连卡死 |
| 5 | 同时间窗对照（#4 vs #2 同时发起） | QG 1.18MB/s，**Mac 单流也能 1.28MB/s** |

## 结论（当前证据支持）

1. **mesh 数据路径本身健康**：QG 视角 1MB/s 稳定；Mac 单流峰值 1.28MB/s。
   排除 jf→deskw、DESK_TJ、elephant 服务端问题。
2. **慢的形态是"每个新连接冷启动 + 并发流互相拖累"**：
   - 一批并发资源请求中首个大资源 ~20s（105KB/s），单独重发同一文件 1.6s（1.28MB/s）；
   - 连续单发也出现 70KB/s→670KB/s 的抖动（新连接 lane/carrier 分配差异）；
   - 用户浏览器开着的 dashboard 本身就在制造并发流量，与诊断流量互相挤占
     （抓包同时段可见多条 fakeIP 流：100.0.15.75/100.0.16.170 等）。
3. 与历史问题同族：`f51dd26 fix(p2p): 批量流量下 SSH 交互小包被 peer 级 credit 饿死`、
   `elephant-download-dribble`、h_tunnel "RTT 节奏合帧"冷启动（v0.8.2/0.8.3 的
   warm lane/carrier 优化多数被负结果回退，见 docs(perf) 系列）。
4. 旁路路径无 NAT（iptables 仅 Docker 规则），回程以真实 IP 193.131→Mac 走
   eth1，抓包确认双向正常；抓包见 TCP 窗口偏小（server win 6466）但非瓶颈主因
   （单流可到 1.28MB/s）。

## 待确认问题清单

### 问题1：坏窗口（10:47-11:00）的成因
**现象**：10:47-11:00 间建连卡死（7s/30s，SYN 重传退避形态）+ 单流 40-150KB/s；
**11:01 自愈**，此后所有探测健康。坏窗口内 deskw 与 jf 的日志均为出厂 error
级别（`util.init()` 默认，v0.8.3-38 起生效），lane/WS/回退事件全部不可见，
**无法事后取证**。
**已排除**：
- [x] mesh 路径带宽：QG 同窗发起 1.0-1.2MB/s 稳定（10:45 实测）
- [x] elephant 服务端：deskw LAN 侧秒回；服务端带 gzip（2.1MB→245KB，8.7×）
- [x] lane 机制故障：11:30 起日志可见，warm2 keeper 正常（秒级补齐 WS lane），
      5min idle 回收→秒级热身，稳态振荡 2↔1↔2，属设计行为
- [x] 当前建连：15/15 探测 0.24-0.65s 无停顿；完整首屏 20 资源 6 并发
      **2.3s**（gzip 后仅 ~700KB 传输量）；90s 双通道探测 mesh 无劣化
**候选成因（按证据强度）**：
1. Mac→QG 入口（WiFi 抖动）：双通道探测显示 LAN ping 有 73ms 尖峰；SYN 在
   WiFi 上丢失会精确产生 7s/30s 重传退避形态；QG 同窗发起正常指向入口段
2. lane 周期瞬间：lane 移除→补齐的秒级窗口内新流被钉到 primary
   （`pickLanePinned`：钉住后不迁移），赶上劣化期则持续慢
3. jf/deskw 侧当日上午 lane 状态异常（无法取证，日志盲区）
**需要验证（若复发）**：
- [ ] 复发时立即抓 QG eth1（macOS SYN 重传）+ deskw p2p 日志（lane 状态）
- [ ] 对比同时刻 Mac→QG 纯 LAN（scp/ping）与 mesh 探测，判断是否同步劣化

**当前状态**: ⚠️ 窗口已自愈，成因未定论；可观测性已修复（见下）

## 结论与修复

**当前状态（11:50 复测）：全路径健康。**

- 完整首屏（20 资源、gzip、6 并发）：**2.3s**，对齐 11-19 deskzq 健康基准
- 建连：15/15 无停顿；90s 双通道探测 mesh 无一次劣化
- 吞吐：单流 1.28MB/s、4 并发聚合 1.85MB/s（≈15Mbps，疑似办公区→JF 私有
  链路（10.140.51.2 方向）的物理天花板；deskzq 站点裸路径为 23Mbps，两个
  站点线路等级不同。后续若要突破需运维评估链路扩容）

**已交付的修复**：

1. **可观测性（d8067a0）**：`util.init()` 将 `p2p`/`htunnel` 模块默认级别提为
   info（与 system/tun 同模式）。此前出厂默认 error 吞掉了 lane 热身/回收、
   WS 建立、HTTP 回退全部诊断，是坏窗口无法取证的根本障碍。随下次机群升级
   生效；deskw 已运行时设置（htunnel=info, p2p=info）。
2. **诊断资产**：本次确认的探测手法——QG eth1 抓包（`tcpdump -e` 看 SYN 方向）、
   `/api/p2p` stripe 状态、`/api/log/modules` 运行时调级（无需重启）、
   `curl --resolve` 绕 macOS DNS 负缓存。

**运维遗留**：
- jf SSH 密码过期（`layer4@36.140.28.178:60002` 无法登录），需人工改密
- deskw 无 OS SSH/自启机制，重启依赖桌面双击（见 qg-udp-fd-leak 文档 11-23 节）
- 办公区→JF 链路 ~15Mbps 天花板：如需 dashboard 更快，优先评估链路扩容或
  elephant 服务端资源体积优化（当前 gzip 已开启）

**复发处置预案**：若用户再次感知慢，立即（1）`curl -w` 分相位探测定位卡在
TCP/TLS/TTFB 哪段；（2）抓 QG eth1；（3）查 deskw/jf 的 p2p/htunnel 日志
（新默认可见）——三步即可定位，不再有取证盲区。

## 更新日志

- 2026-11-23：初版。隔离矩阵 + 抓包（QG eth1）+ 同时间窗对照完成。
