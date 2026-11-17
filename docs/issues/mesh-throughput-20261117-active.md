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

## 下一步修正

- 重点查 jf→gg 承载（~20ms RTT 的 TCP 窗口/节奏）与 jf transit 转发开销，而不是 qg→jf。
- qg 仅 2 vCPU，phaeton 单流已占 ~50–60% CPU；后续多流测试需考虑 qg CPU 上限，
  或用 vm/mb 等具备 TUN 的端点做发起端。

## 备注

- 测试后需删除 `/tmp/bulk64`（qg `/tmp` 为 236MB tmpfs，测试前一度 100% 满）。
- 本次数据为单次结果，后续会追加多次采样与中位数，不覆盖旧数据。
