# P2P Mesh 数据帧稳定性设计

## 目标

消除同一 stream 的并发 frame 写入，确保 reload 不遗留旧 P2P manager，并精确清理失效 session。

## 约束

- 不改 gVisor、NAT、RouteSelector 或 Link NIC 生命周期。
- 不新增 mesh IP 包 ACK、重传或排序协议。
- 控制帧的既有 ACK/重传机制保留。

## 设计

每个 peer 的 `peerWriteLoop` 是唯一 transport writer。普通数据、控制帧、纯 ACK 和控制帧重传均投递到它的优先级队列。`streamTransport` 额外使用写锁保护 frame 头部与 payload 不交错。

`P2PManager.Stop` 必须停止全部 session、重连循环和后台循环，注销 sender 并等待退出。全量 reload 必须在发布新 manager 前停止旧 manager。按 node/link 停止时清理所有匹配 session；发送失败按实际 sender/link 清理。

watchdog 必须按 linkID 查询质量，不能以 linkID 查询 proxy 名索引。
