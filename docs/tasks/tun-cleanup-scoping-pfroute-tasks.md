# TUN 清理作用域收敛 + darwin PF_ROUTE 改造任务

> 设计方案：[tun_cleanup_scoping_and_pfroute_design.md](../plans/tun_cleanup_scoping_and_pfroute_design.md)

## 阶段一：清理行为收敛（darwin/linux）

- [ ] cleanup_darwin.go：删全局 `route delete 0/1+128/1`；只按 tunIP 精确匹配 utun；命中仅 `ifconfig delete <ip>`；不 down 接口；删 198.18/15 兜底
- [ ] cleanup_linux.go：先 netlink 扫 `tun*` 找持有 tunIP 的 link → 按 link index 删 0/1+128/1 路由 + `AddrDel` 删地址（替换 ip 外部命令）；不 link down；删 198.18/15 兜底；resolv.conf/state/iptables 恢复保持无条件
- [ ] 三平台 `go build` 验证

## 阶段二：darwin PF_ROUTE 结构化改造

- [ ] 新增 tun/routesock_darwin.go：PF_ROUTE 封装（routeAddInterface / routeAddGateway / routeDelete / defaultRoute，纯 syscall）
- [ ] route_darwin.go：platformSetup、platformTeardown、deleteExclusionRoute、getDefaultGatewayDarwin 改走 PF_ROUTE
- [ ] 三平台 `go build` 验证

## 验证

- [ ] Mac 实测：kill -9 残留被清；共存 198.18 utun + 0/1 路由不被误伤；TUN 开关路由正确
- [ ] windows `_recreate_check` e2e 通过
- [ ] 提交推送（阶段一、阶段二分开提交）
