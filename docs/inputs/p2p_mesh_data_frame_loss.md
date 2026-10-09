# P2P Mesh 数据帧丢失事实

QG→JF→GG 路径中，QG 与 JF 均记录到数据帧成功入队，GG 经常未收到 `FrameMeshPacket`。TCP 与 DNS 均受影响，排除 gVisor、NAT、RouteSelector 和 Link NIC 生命周期。

当前 `PeerSender.Send` 返回成功仅表示写入本地队列。控制帧重传绕过 peer writer 并发写 stream；旧 P2P manager 可能在 reload 后继续运行；同 node 的失效 session 可残留并参与轮询；watchdog 使用错误身份查询 link 质量。

本次不在 P2P 层重传 IP 数据包。TCP 的重传由网络栈处理，UDP 保持尽力而为。
