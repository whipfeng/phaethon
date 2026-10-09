# Mesh 转发关联追踪任务

- [x] 更新 input、config spec、design 与索引。
- [x] 添加默认关闭的 `mesh.forwarding-trace` 配置。
- [x] 实现只读 IPv4 TCP SYN / DNS 描述符。
- [x] 在 TUN、RouteSelector、Link NIC、MeshManager、P2P 和入站注入边界添加追踪。
- [x] 添加描述符和转发路径测试。
- [x] 运行初始定向测试及 Linux/Windows 构建。
- [ ] 修复所有追踪日志均受 `forwarding-trace` 开关控制的默认关闭约束。
- [ ] 运行定向测试及 Linux/Windows 构建。
- [ ] 部署 VM，验证关闭开关时无新增追踪日志；短时打开并完成一次合格请求后关闭。
- [ ] 部署 JF、GG，保持开关关闭并验证服务和主日志。
- [ ] 部署 QG，确认到 GG 的下一跳为 JF；否则停止，不修改路由。
- [ ] 依次在 GG、JF、QG 短时打开追踪，从 QG TUN/旁路网关发起一次请求到 `gg.phn`，最多观察 60 秒。
- [ ] 只按三端主日志的关联键与时间窗判读首个缺失事件；随后关闭全部追踪并重启节点。
