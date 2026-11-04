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

## 验证（2026-11-04 Mac 真机，mb.qg.phn，验证构建 splitfix2）— 异主 0/1 preempt

- [x] 三平台 go build（windows/linux/darwin 核心包全过）
- [x] phase_d 全通：holder 持有 0/1 时实例 enable 成功（running:true，0/1+128/1 经本 tun）；holder 198.18 地址/路由全程完好；disable 后 holder 的 0/1 被恢复（netstat 复核 UGScg via 198.18.0.1）；最终清理零残留、NET-OK
- [x] 回归 phase_a（开关）/phase_b（kill -9 残留 preempt）通过；bindprobe 矩阵与历史基线一致（localaddr+boundIf(tun) / UDP listen+boundIf 的 ERR 为内核固有形态，dialer 不走这些路径，verify-r4 日志错误 0）
- [x] 探针 rtget_probe 实证根因并验证修复

## 实证过程中修正的设计点

- **ifscope 路由必须带 RTA_NETMASK(0.0.0.0)**：缺 netmask 时内核接收路由且 netstat 显示 UGScIg，但 scoped 查表与 route(8) delete 均 "not in table"。对照实验定位（PF_ROUTE 版失败 vs route(8) 版 OK），补 netmask 后全矩阵通过。
- **xnu RTM_GET 回包 netmask 截断编码**：/1 以 `05 ff ff ff 80` 返回（family=0xff + 仅非零字节），按 AF_INET 解析得 nil 会误判"无冲突"致 preempt 静默失效。sockaddrNetmask 双形态解析修复。

## 遗留（不属于本轮，待用户定夺）

- [x] darwin ifconfig×3 → SIOCAIFADDR / SIOCSIFFLAGS / SIOCDIFADDR — 已完成并验证（见 docs/plans/darwin-audit-hardening.md，含同轮全篇审计 B/C/D/E/L/M 项）
