# Mesh 中转快路径（transit fast path）设计草案

状态：DRAFT，未实现  
背景：`docs/issues/mesh-transfer-throughput-20261109.md` §13。两跳 vm→qg→jf 的单流已从
127KB/s 恢复到 2.0~2.2MB/s，8 流聚合 2.3~3.0MB/s；分段实测 vm→qg 7.72MB/s、qg→jf 4.45MB/s，
两跳低于两段的较小者，差额来自 qg 单 vCPU 上同时做两段加解密 + gVisor 转发。
qg 的 cpuset 被固定在 1 个 CPU（`cpuset.cpus.effective=1`），需要宿主机侧调整，容器内无法改。

## 1. 目标

中转节点对 transit 帧不再注入 gVisor netstack，直接按已算好的 bestPaths 转发到下一跳
P2P 队列，去掉每包一次 netstack 路由/转发成本与相关拷贝。目标：qg 单核两跳从 ~2.3MB/s
提升到 3.5~4.5MB/s。

## 2. 判定条件（只在同时满足时走快路径）

1. 帧为 IPv4，`len >= 20`，`IHL >= 5`；
2. `dstIP` 落在 mesh CIDR 内，且 `routeTable.subnetOwner[dst]` 存在；
3. owner != 本节点（不是本地投递）；
4. `bestPaths[owner]` 存在，`NextHop` 非空且 != `fromNodeID`（避免立刻回弹成环）；
5. TTL `frame[8] > 1`；
6. 能找到到 NextHop 的 `PeerSender`（多链路时按现有质量排序取第一）。

任一条不满足 → 走现有 `linkNIC.InjectInboundOwned` 路径，行为完全不变。

## 3. 转发动作

1. `frame[8]--`；
2. 重算 IPv4 头校验和（只覆盖 `IHL*4`，IHL 最大 60 字节）；
3. 选出的 `PeerSender.Send(frame)`（`Peer.Send` 内部会拷贝，不能依赖帧缓冲生命周期）；
4. 成功后释放 ingress 帧（`releaseOnce`）并 `return`；失败则回退 netstack 注入。

## 4. 明确不做的事

- 不改 TTL 之外的 IP 头字段；不处理 IPv4 options（仅按 IHL 计算校验和）；
- 不做分片/重组；mesh 内 MTU 统一，若后续出现分片需求再扩展；
- 不处理 advertised prefix（非 /16 网段，如 10.x、192.168.x）的快路径，继续交给 netstack；
- 不改变 IPIP 终止逻辑：dst 是本节点 EIP 时 owner==self，仍走 netstack 解封装；
- 不改变 u-turn 统计语义。

## 5. 风险与验证

- 风险：TTL/校验和错误导致丢包；多链路负载均衡退化为首链路；PeerSender 生命周期
  （peer 重连）导致 send 失败，必须有 netstack 回退；帧缓冲所有权错误会导致 use-after-free。
- 验证：
  1. 单元测试：构造 IPv4 帧，校验 TTL 递减与头校验和；
  2. 两节点单跳回归：vm↔qg、qg↔jf 吞吐/连通性不退化；
  3. 两跳 vm→jf 单流/8 流对比（目标 ≥3.5MB/s 且无 stall）；
  4. 观察 `/api/mesh` 的 uturnCount、channelDropStats、tcpStats，确认没有新增丢包/重传；
  5. 灰度：可先加配置开关（默认关闭），在 qg 单节点打开验证后再默认开启。

## 6. 备选方案

先给 qg 增加 vCPU（宿主机 cpuset 调整）。成本最低，预计两跳到 4~6MB/s；
快路径是在无法加核时的软件解法。两者不冲突。
