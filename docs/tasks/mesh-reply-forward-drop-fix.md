# Mesh 转发流 reply 方向丢包修复任务

对应设计：[mesh_reply_forward_drop_fix.md](../plans/mesh_reply_forward_drop_fix.md)

## 阶段 1：毒源实锤 + 修复（D1/D2）

- [x] 实锤：毒源是 gVisor 原生 maybePerformNoopNAT（空地址 noop SNAT 标记 manipPerformed + fork 补丁无条件记录 originalInputNIC），非 SNAT 规则误命中（设计文档有完整证据链）
- [x] 修复：conntrack.go performNAT 仅在 changeAddress=true（真实 SNAT）时记录 originalInputNIC
- [x] 否决并撤回 FindRouteViaNIC fallback 兜底：修复后 OutputNICName 只可能是 tun，兜底为死代码
- [ ] `go build -a` 主二进制 + 本机 VM 部署（一条命令 stop+replace+start），验证基本转发正常

## 阶段 2：上线

- [ ] 用户确认后部署 QG（rc-service phaethon restart）
- [ ] 验证：poison 窗口 FAILED 日志消失、LAN DNS 回归正常、GG probe 长窗口无失败突发
- [ ] 收尾：降 QG 日志级别（保留 gvisor/forward，降 route/mesh/linknic）；杀 catch_poison.sh（PID 26770）；清理 /root/s5*.log 切片与仓库根诊断产物
- [ ] 提交（等用户确认）
