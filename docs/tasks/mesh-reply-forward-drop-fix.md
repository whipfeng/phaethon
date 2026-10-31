# Mesh 转发流 reply 方向丢包修复任务

对应设计：[mesh_reply_forward_drop_fix.md](../plans/mesh_reply_forward_drop_fix.md)

## 阶段 1：毒源实锤 + 修复（D1/D2）

- [x] 实锤：毒源是 gVisor 原生 maybePerformNoopNAT（空地址 noop SNAT 标记 manipPerformed + fork 补丁无条件记录 originalInputNIC），非 SNAT 规则误命中（设计文档有完整证据链）
- [x] 修复：conntrack.go performNAT 仅在 changeAddress=true（真实 SNAT）时记录 originalInputNIC
- [x] 否决并撤回 FindRouteViaNIC fallback 兜底：修复后 OutputNICName 只可能是 tun，兜底为死代码
- [x] `go build -a` 主二进制 + 本机 VM 部署（一条命令 stop+replace+start），验证基本转发正常

## 阶段 2：上线

- [x] 用户确认后部署 QG（v0.8.0-218-gee4c9d5，md5 校验，stop+replace+start 一条命令）
- [x] 验证：新进程启动后 FAILED 计数为 0；gg/jf mesh DNS 10 轮探针 9/10 通（首轮失败为旧进程残留窗口）；真实 SNAT 仅命中 LAN 源（192.168.1.x），mesh 流不再毒化
- [ ] 观察期：长窗口 GG probe 持续无失败突发（jf↔gg 单点 flapping 为已知独立问题）
- [ ] 收尾：降 QG 日志级别（保留 gvisor/forward，降 route/mesh/linknic）；杀 catch_poison.sh；清理 /root/s5*.log 切片与仓库根诊断产物
- [x] 提交：gvisor-fork 7f69f4c + phaethon ee4c9d5（push 待用户确认）
