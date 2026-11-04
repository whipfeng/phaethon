# darwin 转发修复任务

> 设计方案：[darwin_forwarder_fixes_design.md](../plans/darwin_forwarder_fixes_design.md)

## 修复（全部 darwin 限定）

- [x] routesock_darwin.go：routeAddIfscopeDefault / routeDeleteIfscopeDefault / routeGetIfIndex
- [x] route_darwin.go：platformSetup 加 ifscope 物理默认路由，platformTeardown 删除
- [x] dialer/routesock_darwin.go（新）：dialer 自含 PF_ROUTE RTM_GET
- [x] dialer/bind_darwin.go：route-get 去 exec + setBoundIf 按 socket 家族设 sockopt
- [x] device_darwin.go：Read 内 poll 消除 EAGAIN 忙轮询

## 验证（2026-11-04 Mac 真机，mb.qg.phn，验证构建 darwinfix2）

- [x] 三平台 go build（windows/linux/darwin 全过；散装 tools 包 main 重复为存量问题）
- [x] Mac bindprobe 全矩阵：TCP 6 变体全 OK（修前 localaddr+boundIf= no route to host / boundIf(phys)= unreachable）；teardown 后 routes/ifscope/addr 全清
- [x] Mac 控制台/文件日志两类错误归零（no route to host=0、invalid argument=0），nslookup/dscacheutil 经 mesh DNS(100.0.0.3) 解析成功，baidu/apple 200
- [x] Mac enable 后 CPU 0.0%（修前 100%，总 CPU time 流量前后不变）
- [x] Mac disable 后 ifscope 默认路由删除（netstat UGScIg 消失）
- [x] 回归 phase_a（开关）/phase_b（kill -9）通过；EEXIST 路径专项：kill -9 残留 ifscope → 重启再 enable 计数=1（去重生效）→ disable 清除
- [x] phase_d：holder 198.18 地址/路由完整（cleanup 跳过正确）

## 实证过程中修正的设计点

- **ifscope 路由必须带 RTA_NETMASK(0.0.0.0)**：缺 netmask 时内核接收路由且 netstat 显示 UGScIg，但 scoped 查表与 route(8) delete 均 "not in table"。对照实验定位（PF_ROUTE 版失败 vs route(8) 版 OK），补 netmask 后全矩阵通过。

## 遗留（不属于本轮，待用户定夺）

- [ ] **EEXIST 异主 0/1**：phase_d 中 holder 持有 0/1 via 198.18.0.1 时，实例 enable 因 `route add 0/1: file exists` 失败（phase_d 的 holder 保护目标已通过；实例与 holder 共存策略待决策）。关联：defaultRoute() RTM_GET 0.0.0.0 在 0/1 存在时会命中 0/1 而非真默认（netstat 解析时代无此问题），决策时需一并考虑。
- [ ] darwin ifconfig×3 → SIOCAIFADDR / SIOCSIFFLAGS / RTM_DELADDR（cleanup 的 mask 匹配语义需专项测试）
