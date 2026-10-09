# QG → JF Mesh 转发关联诊断需求

日期：2026-10-09

## 现象

QG 访问 JF/GG mesh 地址间歇超时。QG 本机访问 `qg.phn` 正常，说明本地 LocalStack 路径可用；跨节点目标必须经过 RouteSelector Branch 2、Link NIC、P2P 与底层传输，现有日志无法定位首个缺失阶段。

QG → JF 使用 h_tunnel v1 BIND PORT=2。JF 已记录 BIND 建链、HELLO；QG 的控制 ACK 样本持续存在，但控制面正常不能证明 `FrameMeshPacket` 数据面投递。

## 排除项

- 不为 mesh 数据包增加 ACK、重传、排序或可靠投递协议。
- JF 未启用 TUN；在 JF 本机对 mesh 地址执行 curl 走 OS 默认路由，不能作为 mesh 数据面验证。
- 不修改 RouteSelector、NAT、Link NIC 或 h_tunnel 的转发语义。

## 需求

增加默认关闭、仅观测的跨层关联追踪，能够按照单个 TCP 初始 SYN 或 DNS 包辨识：TUN 注入、路由决定、Link NIC、peer 选择、P2P 队列/写入、远端接收和 Link NIC 注入的首个断点。
