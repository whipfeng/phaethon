# Mesh 转发关联追踪任务

- [ ] 更新 input、config spec、design 与索引。
- [ ] 添加默认关闭的 `mesh.forwarding-trace` 配置。
- [ ] 实现只读 IPv4 TCP SYN / DNS 描述符。
- [ ] 在 TUN、RouteSelector、Link NIC、MeshManager、P2P 和入站注入边界添加追踪。
- [ ] 添加描述符、RouteSelector、P2P 队列和 h_tunnel v1 FrameTransport 测试。
- [ ] 运行定向测试及 Linux/Windows 构建。
- [ ] 先部署 VM，再在 QG/JF/GG 短时启用追踪并捕获断点，随后关闭。
