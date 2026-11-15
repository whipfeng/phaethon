# SSH 经 GG 代理入口访问 VM：批量流量下输入卡顿（2026-11-15）

## 现象

从 GG 的代理入口访问 VM 时，SSH 交互偶尔“输入没反应”。经实机复现，触发条件是**同一条 mesh 链路上有批量流量**（例如经同一入口上传大文件），不是单纯的 idle。

## 复现环境

- GG 本机 → trojan 入口 `127.0.0.1:39988`（SNI `www.dynamictj.com`）→ `172.30.0.2:22`（VM 的 OS sshd）
- 路径：`gg → jf → qg → vm`，三跳，IPIP 通告路由
- 交互流：一个 PTY SSH 会话，每秒发送 `MARK_n` 并等待回显
- 批量流：300MB 上传 `dd if=/dev/zero bs=1M count=300 | ssh ... 'dd of=/dev/null'`

## 测量结果

| 场景 | 交互输入回显延迟 |
| --- | --- |
| 空闲 | ~100ms |
| 300MB 批量上传并发 | min 164ms / p50 498ms / p95 1370ms / max 1385ms |

批量上传期间 `[P2P-QDEPTH]`：

```
peer=5 proxy= queued=0KB drrOut=77 ctrl=0 maxSojourn=1.124s
writes=230 busy=3ms maxWrite=0s rate=129.2KB/s
```

- `maxSojourn=1.124s`：帧从入队到写出等待了 1.1 秒；
- `maxWrite=0s`、`busy=3ms`：实际写 socket 并不慢；
- `queued=0KB`：DRR 数据队列本身没有积压；
- 说明帧卡在 **peerWriteLoop 的在途 credit 门控**，不是网络或 socket 写阻塞。

## 根因

`p2p.peerWriteLoop` 使用 **peer 级**的在途 credit（`stripe.writeCredit()`）。批量流把 credit 用满后，write loop 停止消费 `drrOutput`，所有数据流（包括交互 SSH 的小包）一起等待；等 credit 恢复时，交互包前面已经排着批量流的在途窗口，形成 1 秒级卡顿。控制/DNS 有旁路，但交互数据没有。

## 修复

新增 `smallCh` 小数据优先队列：

1. `isSmallInteractiveData` 识别 SSH（端口 22）的**小数据**帧（≤512B、非纯 ACK）；IPIP 按内层包判断。
2. `drrLoop` 把小数据帧投递到 `smallCh`；队列满时回退 `drrOutput`，不阻塞调度器。
3. `peerWriteLoop`：
   - credit 充足时，批次顺序为 control → DNS → small → bulk；
   - credit 耗尽时，仍可发送 control/DNS/small，并允许 **64KB 保留额度**；额度用完才等待 credit 恢复；
   - 批量数据继续受原 credit 门控，不影响吞吐策略。
4. 纯 ACK 和非 SSH 流量保持原 DRR 路径，避免跨类乱序。

## 预期

批量上传并发时，交互 SSH 的小数据不再排在批量在途窗口之后，p95 应从 1.3s 量级回落到百毫秒级；批量吞吐仍由原 credit/DRR 控制。

## 验证状态

- 代码 + 单测完成（`p2p` 全部通过）。
- 待部署 702 到 gg/jf/qg/vm 后重跑同一批量上传 + 交互 PTY 测试，记录修复后延迟。
