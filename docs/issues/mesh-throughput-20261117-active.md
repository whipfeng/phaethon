# 2026-11-17 主动吞吐复现（qg ↔ mb / qg ↔ gg）

状态：进行中  
环境：v0.8.1-94/95（Linux/mac v94，Windows v95；代码一致）  
方法：同一窗口、同一 46.7MB 文件（`/tmp/bulk64`，实际 48,943,104 字节），
`ssh <target> "cat > /dev/null"` 作为数据 sink；OS 与 mesh 都走 SSH，
因此 SSH 加解密开销在两侧一致，差异来自承载/转发路径。

## 结果（单次，非多次平均）

| 路径 | 数据量 | 耗时 | 吞吐 |
|---|---:|---:|---:|
| OS qg→mb（LAN 192.168.1.88） | 46.7MB | 8.75s | 5.3 MB/s |
| Mesh qg→mb（1 跳，MB_HT） | 46.7MB | 15.12s | 3.1 MB/s |
| Mesh qg→mb 8 流（8×8MB） | 64MB | 21s | 3.05 MB/s |
| Mesh qg→jf（1 跳） | 46.7MB | 9.01s | 5.2 MB/s |
| OS qg→gg（公网 106.13.183.103） | 46.7MB | 40.32s | 1.16 MB/s |
| Mesh qg→gg（2 跳，qg→jf→gg） | 46.7MB | 126.1s | 0.37 MB/s |

## 观察

1. 1 跳 mesh（qg→mb）比 OS 直连慢约 1.7×；qg→jf 1 跳与 OS 直连接近（5.2 vs 5.3 MB/s）。
2. qg→mb 8 流总吞吐与单流几乎相同（3.05 vs 3.1 MB/s），说明 mb 方向存在**共享瓶颈**，
   不是单流窗口/单流拥塞控制限制。
3. 2 跳 qg→gg 仅 0.37 MB/s，比“两段 1 跳串联”的估算（~0.95 MB/s）还低约 2.5×，
   relay 路径存在额外开销。
4. 当前未观察到 >3s 的停滞；但 mb 方向 3 MB/s 与 8 流不扩展是明确问题。

## 下一步（待做）

- 用 mesh 控制台 HTTPS 下载同一 9MB 包（1 流/8 流），剥离 SSH 加解密，判断瓶颈在
  SSH server 还是承载/转发。
- 分别测 qg→jf、jf→gg 两段单跳吞吐，定位 2 跳额外开销。
- 用 qg→vm（UDP leg）重复同样测试，确认 UDP leg 是否同样存在共享上限。
- 主动触发：在大流量期间并发小包交互，记录 RTT 尾部与 leg 计数冻结。

## 追加：剥离 SSH 的 HTTP 测试（同一 9.46MB 包）

| 路径 | 1 流 | 8 流（75.7MB） | 结论 |
|---|---:|---:|---|
| OS qg→mb:39999 | 6.73 MB/s (1.41s) | 13s → 5.8 MB/s | 8 流不扩展 |
| Mesh qg→mb:443 | 2.85 MB/s (3.32s) | 28s → 2.7 MB/s | 8 流不扩展 |

- mesh 1 流比 OS 慢 2.36×；
- OS 与 mesh 的 8 流都不扩展，说明 **mb 端点/HTTP/TLS 或 qg 客户端 CPU** 也是共享瓶颈，
  不能只用 mb 的 8 流结果判断 mesh lane 是否扩展。

## 追加：qg→jf 8 流

- mesh SSH 8×8MB（64MB）到 jf：12s → **5.33 MB/s**，与 1 流 5.2 MB/s 基本相同。
- qg 为 2 vCPU；1 流传输时 phaeton 约 47–57% CPU，整体 23% idle。
- qg 的 CPU/加解密是绝对吞吐的潜在上限；8 流不扩展在 OS HTTP 上也出现。

## 追加：jf/gg 是 transit-only（无 phaeton TUN）

- jf：`ip -br addr | grep tun` 只有 K8s `tunl0`，无 phaeton TUN；`ip route get 100.179.0.10`
  走 `via 10.140.51.1 dev eth0`。
- gg：无 phaeton TUN；`ip route get 100.2.0.10` 走 `via 192.168.16.1 dev eth0`。
- 因此 jf↔gg 的本机发起测试无效（超时是路由未进 TUN，不是链路故障）；qg→gg 的
  2 跳数据由 jf 的 transit fast path 转发，不依赖 jf 的 TUN。

## 两跳分解（推导）

设 qg→jf 为 5.2 MB/s，qg→jf→gg 为 0.37 MB/s，串联模型：
`1/0.37 ≈ 1/5.2 + 1/x` → `x ≈ 0.40 MB/s`。
即 **jf→gg 段约 0.40 MB/s**，而 gg 的 OS 公网基线为 1.16 MB/s，mesh 段慢约 3×。
qg→jf 跳本身已接近 OS 直连，2 跳的主要差距在 jf→gg 承载/转发段。

## 追加：24.5MB 同方法复测（修正"回归"判断）

用历史同款 24.5MB 单流、同一 qg→jf / qg→mb（mesh IP）复测：

| 路径 | 24.5MB 耗时 | 吞吐 | 历史基线（2026-11-12） |
|---|---:|---:|---:|
| qg→jf（1 跳） | 3.10s | **8.29 MB/s** | 8.9 MB/s |
| qg→mb（1 跳） | 6.77s | **3.79 MB/s** | vm→qg→mb 2 跳 3.2 MB/s |

结论修正：
- **基础单跳吞吐没有回归**，qg→jf 8.29 已回到历史 8.9 的水平；qg→mb 3.79 还高于历史 3.2。
- 先前 46.7MB 的 5.2 / 3.1 MB/s 是**长传输平均**，包含慢簇/停顿，拉低了均值；
  这与历史文档里的"慢簇/离群"是同一现象，不是基础码率退化。
- 因此当前剩余目标是**长传输中的偶发慢簇**（共享 mesh 载体/HOL），不是单跳基础速率。
- qg 上 `jf.phn`/`mb.phn` 节点域名解析在本次重启后失败（`git.migu.cn` 域名路由正常），
  测试改用 mesh IP；该 DNS 分支单独排查，不与性能结论混在一起。

## 追加：qg→jf 46.7MB ×10（当前 v94 环境）

`5.63 / 6.38 / 5.34 / 5.92 / 6.84 / 6.67 / 6.80 / 7.22 / 6.31 / 13.20s`

- 9 次 5.3–7.2s（7.0–9.2 MB/s），1 次 13.2s（3.7 MB/s）；
- 中位 ~6.5s ≈ 7.5 MB/s；离群约 10%，与历史“慢簇/离群 ~10%”一致；
- **基础单跳没有回归**，剩余是共享载体（qg↔jf 的 WS/nginx LB）的偶发慢簇。

GG 路径：`0.46–0.63 MB/s`；文档结论是 GG 出口约 3Mbps（0.375MB/s），当前已在容量附近，
**不作为代码吞吐验收路径**（验收用 vm→qg→mb / DESKW）。

节点域名 DNS：domain trie 收敛后恢复（`jf.phn → 100.2.0.10`），此前失败是重启后的短暂窗口，
不是持续 bug。

## 追加：per-carrier writer A/B（qg→jf，24.5MB 同窗口）

| 构建 | 5 次耗时 | 中位 | 吞吐 |
|---|---|---:|---:|
| v94 pre | 4.21 / 2.84 / 2.67 / 3.13 / 2.72s | 2.84s | 9.05 MB/s |
| v101 depth=4 | 4.08 / 3.52 / 3.06 / 3.09 / 3.34s | 3.34s | 7.69 MB/s |
| v101 depth=32 | 2.87 / 2.80 / 2.71 / 2.98 / 2.82s | 2.82s | 9.11 MB/s |

结论：depth=4 有 ~15% 中位损失（尾丢弃 → 内层重传）；depth=32 恢复到 pre 水平。

46.7MB 观察：
- depth=4：6.2–7.8s，未出现 13.2s 离群；交互探针 0.20–0.36s。
- depth=32：6.37–8.98s，交互探针基线 0.4–0.7s、一次 1.58s——同一窗口探针基线整体抬升，
  说明该窗口 qg↔jf 的 nginx/LB 链路更差，不能归因于该改动。

最终选择：**保留 per-carrier writer，primaryDataDepth=32**；qg↔jf 的 46.7MB 波动仍由
nginx/LB 链路窗口主导，代码侧只能保证 primary 写 HOL 不扩散到健康 lane。

## 最终发布：v0.8.2 全量

- per-carrier writer 恢复，primaryDataDepth=32；
- 9 个节点全部 `v0.8.2`：qg/gg/jf/ms9/ms10/mb/vm/deskzq/qgw（deskzq 于 2026-11-18 恢复）；
- qg→jf 24.5MB 冷启动后 2.65–2.95s（≈8.7–9.7MB/s），与 pre v94 中位 2.84s 持平；
- 46.7MB 的波动由 qg↔jf nginx/LB 链路窗口主导（交互探针基线同步变化），不是本改动；
- 有效验收路径 vm→qg→mb 当前 3.79MB/s，高于历史 OS 两跳 3.01 / mesh 3.2；
- GG 路径按文档为 ~3Mbps 出口上限，不作为代码吞吐验收路径；
- 节点域名 DNS 在 domain trie 收敛后正常（jf.phn/vm.phn/git.migu.cn 均通过）。

结论：有效单跳/两跳已接近或超过历史 OS 基线；per-carrier writer 解决 primary 写 HOL
对交互/lane 的扩散，深度 32 保证吞吐不退化；剩余 46.7MB 离群属于 nginx/LB 共享链路窗口。

## 偶发慢窗口（专项记录）

**现象**：qg→jf 46.7MB 偶发单次 13.2s（约 10%），而同期 24.5MB 仍在 2.8–3.0s；
慢窗口期间交互探针基线从 ~0.2s 抬升到 0.4–0.7s（偶发 1.58s）。

**已记录的数据**：
- v94（pre）10 次：`5.63/6.38/5.34/5.92/6.84/6.67/6.80/7.22/6.31/13.20s`；
- v103 per-carrier writer depth=32 30 次：`5.54–8.48s`，无 13.2s 级离群；
- depth=32 的 46.7MB 窗口探针同步抬升，说明该窗口链路本身变差，不能归因于代码。

**判定**：慢窗口与 qg↔jf 的 nginx/LB 共享链路窗口同步；代码侧已通过 per-carrier writer
把 primary 写 HOL 与健康 lane 隔离，剩余收益需要 LB/链路侧权限。

**后续**：继续保留长传输采样；出现慢窗口时同时记录交互探针、leg 计数、srtt/retrans、CPU，
避免把链路窗口误判为代码回归。

## 最终定位（2026-11-18）：qg 实验 env 是慢窗口主因

qg 服务此前带有实验环境变量：

```
PHAETHON_CWND_BDP_GAIN=16
PHAETHON_LANE_DIAG=1
PHAETHON_CARRIER_STALL_MS=2000
PHAETHON_INNER_MAX_RTO_MS=1000
```

移除全部实验 env（使用默认）后，同路径同窗口复测：

| 测试 | 结果 |
|---|---|
| 300MB qg→jf | **31s（~10.1MB/s，rc=0）**；此前同路径 180s 未完成（stall） |
| 10×46.7MB qg→jf | 4.83 / 5.15 / 5.42 / 4.98 / 5.09 / 5.06 / 5.24 / 5.19 / 5.33 / 5.24s，中位 **~5.15s（~9.5MB/s）**，**无 13.2s 离群** |
| 交互探针 | 尾部 0.18–0.55s，少量 0.6–1.1s；批量结束后回落到 0.18–0.19s |

**结论修正**：46.7MB 慢窗口不是 nginx/LB 的必然结果；主因是 qg 的实验 env 组合导致
carrier/lane 行为异常（BDP 16 + lane diag + stall/RTO）。生产 qg 已改为默认 env，
单跳吞吐恢复到 ~9.5–10.1MB/s（历史 mesh 8.9 / OS 7.9–10.1），慢窗口消失。

**采样注意**：per-second 对 66MB 日志做 `grep` 会把 qg 打入 I/O 饱和（100% io、D 状态），
本身上造成 stall；复现时改为只采 `ss -tin` + `top`，日志在测试后一次性分析。

## 备注

- 测试后需删除 `/tmp/bulk64`（qg `/tmp` 为 236MB tmpfs，测试前一度 100% 满）。
- 本次数据为单次结果，后续会追加多次采样与中位数，不覆盖旧数据。
