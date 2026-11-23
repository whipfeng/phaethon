# elephant 仪表盘经旁路网关访问奇慢（2026-11-23）

**创建时间**: 2026-11-23
**状态**: 诊断中（隔离矩阵完成，根因待实锤）
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

### 问题1：新连接慢启动的准确机制
**假设**：每个新 TCP 经 mesh 转发时，lane/carrier 需要建立/预热（h_tunnel HTTP
节奏 + DRR credit 初始窗口），首屏并发时全部流量挤在未预热状态互相饿死。
**需要验证**：
- [ ] QG/jf 日志：慢连接的 lane 分配、carrier 建立、DRR 调度时间线（加诊断标签复测）
- [ ] 关闭浏览器后台流量后单独压测，区分"竞争饿死" vs "纯冷启动"
- [ ] 对比 single-flow 长下载的速率爬坡曲线（是否 10-20s 才爬到 1MB/s）

**诊断方法**：
1. QG 日志加 `[ELEPHANT-PROBE]` 标签跟踪单流 lane 分配；复测下载并 grep 时间线
2. 对比 jf 的 DESK_TJ leg 在并发前后的 framesSent 速率

**当前状态**: ❌ 未验证

## 结论与修复

（待验证后补充；修复方向若确认为 lane 冷启动/公平性，需按流程先提方案——
如恢复 warm lane 预热、调整 DRR credit 初始窗口、或 elephant 场景专用 lane 保留，
涉及 netstack/P2P 调度，须先改方案再改代码。）

## 更新日志

- 2026-11-23：初版。隔离矩阵 + 抓包（QG eth1）+ 同时间窗对照完成。
