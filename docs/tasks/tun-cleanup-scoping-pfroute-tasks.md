# TUN 清理作用域收敛 + darwin PF_ROUTE 改造任务

> 设计方案：[tun_cleanup_scoping_and_pfroute_design.md](../plans/tun_cleanup_scoping_and_pfroute_design.md)

## 阶段一：清理行为收敛（darwin/linux）

- [x] cleanup_darwin.go：删全局 `route delete 0/1+128/1`；只按 tunIP 精确匹配 utun；命中仅 `ifconfig delete <ip>`；不 down 接口；删 198.18/15 兜底
- [x] cleanup_linux.go：先 netlink 扫 `tun*` 找持有 tunIP 的 link → 按 link index 删 0/1+128/1 路由 + `AddrDel` 删地址（替换 ip 外部命令）；不 link down；删 198.18/15 兜底；resolv.conf/state/iptables 恢复保持无条件
- [x] 三平台 `go build` 验证

## 阶段二：darwin PF_ROUTE 结构化改造

- [x] 新增 tun/routesock_darwin.go：PF_ROUTE 封装（routeAddInterface / routeAddGateway / routeDelete / defaultRoute，纯 syscall）
- [x] route_darwin.go：platformSetup、platformTeardown、deleteExclusionRoute、getDefaultGatewayDarwin 改走 PF_ROUTE
- [x] 三平台 `go build` 验证（含 darwin/arm64 的 92 字节结构体断言）

## 验证

- [x] Mac 隔离探针实测：PF_ROUTE 路由形态与 route(8) 逐字节一致（PRCLONING+全填 sdl）、包真实进入 utun、排除路由增删、defaultRoute 解析 192.168.1.101/en0 正确
- [x] Mac 真机 teardown 实测：停实例后 0/1+128/1 自动删除、utun 消失、网络恢复
- [x] Mac 真机 TUN 开关实测（2026-11-04，干净环境）：enable 后 0/1+128.0/1 均加入（netstat 显示为 `0/1`、`128.0/1`）、disable 后路由与地址全删。注意 netstat 格式是 `128.0/1` 而非 `128/1`，grep 易漏
- [x] Mac 真机 kill -9 实测（2026-11-04）：pkill 全杀后 darwin 内核随 fd 释放自动回收 utun 接口、地址与 0/1+128.0/1 路由（3 秒内消失）；重启实例拿到复用 unit，状态干净。CleanupResidual 作为兜底保留
- [x] Mac 真机 198.18 共存实测（2026-11-04，holder 进程持有 utun+198.18.0.1+198.18/16+0/1）：phaethon 启动 + CleanupResidual 后 holder 的接口、地址、路由全部完好，cleanup 无误伤
- [ ] **已知问题（待方案）**：当系统已存在异主 0/1 路由（如其他 VPN/手工注入，gateway 指向别的接口）时，platformSetup 的 routeAddNetViaInterface 报 EEXIST（`route add 0.0.0.0/0/1 via utunN: file exists`），TUN 启动失败。与 cleanup"0/1 存在则不动"的收敛设计存在意图矛盾；且阻止了同机双实例抢占（有保护作用）。处置方案待讨论
- [ ] windows `_recreate_check` e2e：本 PR 对 windows 仅 CleanupResidual 签名对齐（行为零改动，git diff 证实），且唯一 Windows 环境是生产 VM（cleanup 会删生产 TUN 适配器，不可跑）。留待 windows 行为有实际改动时验证
- [x] 提交推送（阶段一、阶段二分开提交；阶段二修复=edc3e2a）
