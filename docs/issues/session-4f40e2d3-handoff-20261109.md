# 会话 4f40e2d3 交接记录（上下文不可压缩导致中断）

> 创建日期: 2026-11-09
> 故障会话: `4f40e2d3-6300-45e2-8cc3-f5e6ac47bde8`（标题「继续啊」，01:06 开始，10:58 终止）
> 关联文档: [full-code-audit-20261109.md](full-code-audit-20261109.md)、[security-hardening-batch1.md](../plans/security-hardening-batch1.md)、[security-hardening-batch1-tasks.md](../tasks/security-hardening-batch1-tasks.md)

## 0. 铁律

**每个批次（plan → 实现 → 验证）做完必须立刻 commit，禁止攒批。**

本次事故的放大器就是这个：最后提交停在 00:19:04，而决策（07:44–09:45）与三个批次的实现（09:45–10:51）全部未提交，一次 rewind 就只剩一个 10:02 的检查点。补充约定：

- 批次内允许 WIP，但「编译 + 单测通过」即为提交点，不等到部署验证。
- 长会话在上下文吃紧前先提交，再决定是否继续。
- 恢复/抢救性改动同样单独提交，并在 message 里写明来源。


## 1. 会话为什么死了

不是网络或工具故障，是**压缩死锁**：

| 指标 | 数值 |
|---|---|
| `events.jsonl` | 637,688,913 字节 / 5010 事件 |
| 会话文本体积（tool_call_output + tool_call + message） | 43.73 MB + 5.14 MB + 4.65 MB ≈ 53.5 MB |
| 压缩尝试 | 4 次，**全部失败**（10:51:20 / 10:53:48 / 10:56:41 / 10:58:10） |
| 失败原文 | `stream processing error: We're sorry, your context length has exceeded the maximum limit.` |
| 内部重试 | `compact_ptl_retry attempt=1 remaining_messages=1421`，随后同样超限 |
| 结果 | `compaction_end {"compacted": false, "error_message": "compact failed after retries: ..."}` |

机制：压缩动作本身要把历史发给模型做摘要，历史已 ~53MB（千万级 token），所以「压缩」自己先超限，任何重试都无效。这个会话从始至终**一次都没压缩成功过**，历史只增不减。

放大因素：
- `state_update` 事件 1318 个、共 551 MB，其中 `file_track_status` 每次都记录**整份文件内容**（单条最大 1.4 MB），反复重读 `gvisor-fork/pkg/buffer/*.go` 这类大文件把它撑爆。
- `tool_call_output` 1263 条 / 43.7 MB，单条最大 396 KB（大文件整篇 Read 的结果）。

## 2. 更严重的副作用：10:56 的 rewind 回滚了三批代码

10:56:18 发生一次 rewind（`target_event_id` = line 3826，即 10:02:15 那条用户消息），`rewind_code=true`，回滚了 **10:02:15 之后的所有编辑**。

当前工作区实测状态（已核对特征符号）：
- `db/matcher.go` 无快照机制（`currentSnapshot`/`atomic.Value` 均不存在）
- `p2p/`、`mesh/`、`tun/`、`dialer/ssh.go` 均无会话期改动
- `gvisor-fork` 工作区干净（`MakeWithExternalData` 不存在）
- `git status` 只剩批次 1 + watchdog/实例接管那批修改

**结论：批次 2（数据面）、批次 3（TUN 生命周期）、批次 4（健康检查与拨号，刚起步）的代码与设计文档全部丢失，未提交。**（已于 2026-11-09 按 §6 从会话事件快照恢复，恢复后编译/交叉编译/单测通过。）

丢失的文档（rewind 时被当作新建文件删除）：
`docs/plans/datapath-correctness-batch2.md`、`docs/tasks/datapath-correctness-batch2-tasks.md`、
`docs/plans/tun-lifecycle-batch3.md`、`docs/tasks/tun-lifecycle-batch3-tasks.md`、
`docs/plans/health-dial-batch4.md`、`docs/tasks/health-dial-batch4-tasks.md`

## 3. 工作区现存内容（未提交）

改动的跟踪文件：
- 批次 1（安全止血）：`admin/admin.go`、`admin/package.go`、`admin/templates/package.html`、`admin/templates/setup.html`、`config/config.go`、`server/{base,control_server,direct,http,listener,socks5,trojan}.go`
- watchdog / 实例接管 / 残留清理：`main.go`、`main_engine.go`、`main_tun_nows.go`、`main_tun_windows.go`、`worker_attr_linux.go`、`docs/plans/watchdog-residual-kill.md`、`docs/issues/pending-items-audit-20261108.md`
- 文档：`docs/index.md`

未跟踪的新文件：
- `instance_lock*.go`、`instance_proc*.go`、`instance_takeover*.go`、`child_signal*.go`（实例锁 / 接管 / 子进程信号）
- `docs/issues/full-code-audit-20261109.md`（全量审计，60 处 D 项，**这是重做批次 2-4 的依据**）
- `docs/plans/security-hardening-batch1.md`、`docs/tasks/security-hardening-batch1-tasks.md`

验证：`go build .` 通过；`go build ./...` 仍因仓库自带 `tools/` 多 main 包报错（历史问题，非本次引入）。

## 4. 批次 1 状态（唯一完整保留的一批）

按 [security-hardening-batch1-tasks.md](../tasks/security-hardening-batch1-tasks.md)：
- 阶段 1-5 全部 `[x]`：包分发改手动升级 + pin、debug API 默认关闭、`listen-addr`、注册端口收紧、bcrypt + 独立 sessionSecret 会话、接口脱敏、trojan 日志脱敏、setup 页兼容
- 阶段 6：构建 / 三平台交叉编译 / vet / 单测均通过
- 仅剩 1 个已知缺口：动态 listener 监听地址仍为 `0.0.0.0`
- **阶段 6.3 部署验证全部未做**（VM 上传包、激活版本、debug API 404、`listen-addr: 127.0.0.1`、改密失效）

## 5. 丢失批次的内容清单（用于恢复或重做）

### 批次 2：数据面正确性（D7/D9/D10/D21/D11/D6）
- D7 matcher 快照：写路径发布不可变快照，`Resolve*`/`resolveName`/组回调全部无锁读快照
- D9 P2P 清理串行化：删 peer 时锁内只摘表，mesh 下线 / 删 NIC 交给专用清理协程
- D10 按流整组路由：lane 入队非阻塞，满/失效整组回退主链路并把该流固定，删掉 50ms 写路径等待
- D21 入站队列：数据帧满队列直接丢，入站全局 64 MB 字节预算，DNS/控制帧独立优先队列
- D11 资源上限：最大入站会话 64（`PHAETHON_MAX_P2P_SESSIONS` 可覆盖），每会话 lane 上限 8
- D6 出站泄漏：TUN 写路径补 `buf.Release()`；link_nic / htunnel 改为「复制载荷 + 释放 ToBuffer 引用」
- 复盘补齐：`onConfigChange` 组回调改读快照、`StartPeer` defer 移出 `m.mu`、`ensureDataQueues/resetDataQueues`（空闲会话不再预占 ~8MB）、stripe 关闭归还池化缓冲、netstack 丢弃分类计数、入站真零拷贝（`buffer.MakeWithExternalData` + `LinkNIC.InjectInboundOwned` + `MeshManager.HandleMeshFrameOwned`）
- **重要更正**：审计里「链路端点必须 DecRef 传入的包」是错的——该 fork 的 `udp/endpoint.go:470` 有 `defer pkt.DecRef()`，加了会 double decrement panic；只释放 `ToBuffer` 引用。

### 批次 3：TUN 生命周期与清理（D8/D19/D20）
- D8 状态机：`Manager` 增 Idle/Starting/Running/Stopping + `lifecycleMu`，非法转换拒绝；`WriteLoopCloseCh` 改为仅 `StopStack` 关闭的稳定通道（关→开 TUN 后出向不再永久断）；启动记录 `dnsIface`、停止用同一名字恢复；启动失败 `TeardownForce()` 逆序回滚
- D19 Windows：适配器删除按 FriendlyName 严格匹配；残留路由按 LUID 过滤；`StopTUN` 后删 `phaethontun`；TUN 禁用/不可用也清理；weak-host / 物理口 DNS 原值存 `data/state/windows-tun.json`，崩溃按原值恢复
- D20 Linux：`resolvectl`/`nmcli` 原值存 `data/state/linux-dns.json`，正常停止与 `CleanupResidual` 走同一恢复；拆分/排除/mesh 路由统一打 `Protocol=200`；非 bypass 不再把 `rp_filter`/`ip_forward` 写 0；评估后**不做 iptables 专用链**（风险高于收益）
- D20 macOS：抢占的第三方 0/1 路由快照存 `data/state/darwin-foreign-splits.json`，崩溃后按快照恢复（带接口名/index 复用校验），正常 teardown 后删除快照

### 批次 4：健康检查与拨号（刚起步就被回滚）
- 设计范围：配置/运行态分离、订阅刷新并发、SSH 客户端池（`sshClientEntry`/`sshClientCache` 常驻与 reap 保护）、ctx 贯穿、D25 版本范围协商（`P2PProtocolVersion`/`MinProtocolVersion` + `protocolCompatible`）
- 当时只改到 `main.go`、`admin/admin.go`、`dialer/ssh.go`、`mesh/topology.go`、`p2p/p2p.go` 的一半
- 参考已完成的相关提交：`675d39d feat(dialer): 健康检查支持取消与按类型超时`

## 6. 恢复结果（已完成）

**恢复源**：`events.jsonl` 里最后一个 `state_update`（10:51:07）带有 `file_track_status`——**86 个文件的完整内容快照**（含 `db/matcher.go` 的 10:17 版本、`p2p/p2p.go` 的 10:51 版本等）。它比「重放编辑」更可靠：快照就是文件原文，不需要匹配 `old_string`。

**执行**：
1. 备份：`git stash create` → `088d9c9`（未被引用，需要时用该 SHA 取回）；另有 31 份被覆盖文件的原内容留在 `%TEMP%\handoff\backup\`。
2. 用快照写回 43 个文件（含 6 篇被 rewind 删除的 plan/tasks 文档 + `tun/adapter_cleanup_*`、`tun_state_windows.go`、`tun/dns_backup_*`、`foreign_splits_darwin.go`），其余文件快照与磁盘一致、未改动。
3. `gofmt` 归一（保留各文件原有 CRLF/LF 风格）。

**验证**：`go build .` ✔、`GOOS=linux`/`darwin` 交叉编译 ✔、`go vet .` ✔、`go test ./db/... ./p2p/... ./admin/... ./server/...` ✔、`go test -vet=off ./mesh/...` ✔、`gvisor-fork/pkg/buffer` 编译 ✔。

**恢复过程中发现并修掉的真实 bug**（原会话代码自带）：

- `p2p/p2p.go` `DRRScheduler.NextBatch` 在函数入口只取一次 `s.getQueues()`。D11 把数据队列改成「首个数据帧才分配」后，drrLoop 拿到的是空占位队列，分配后也永远看不到真实队列 → 出向完全不发包 → 会话被判死、传输被关闭。表现为 `TestHTunnelMeshChannel_E2E` 稳定失败（`read header fail: io: read/write on closed pipe`）。已改为每轮读取队列指针（`drainRound` 内取），测试恢复通过。

## 6.1 恢复后的设计符合性审查

| 批次 | 结论 |
|---|---|
| 批次 1 安全止血 | 完整（阶段 1-5 全勾，阶段 6.3 部署验证未做） |
| 批次 2 数据面（D7/D9/D10/D11/D21/D6） | 完整。符号与路径逐项核对：`ruleSnapshot`+`publishLocked`+`lookup*`（matcher）、`cleanupCh`/`cleanupLoop`/`enqueuePeerCleanup`（p2p）、`laneEnqueue` 非阻塞+`repinPrimary`、`meshInboundByteBudget`(64MB)+`maxInboundSessions`(64)+`env` 覆盖、`stripeMaxLanes`=8、`ensureDataQueues/resetDataQueues`、`MakeWithExternalData`+`InjectInboundOwned`+`HandleMeshFrameOwned`、`sendFailures`、`WriteLoopNonIPv4Drops/WriteLoopNoDeviceDrops` 均在位；唯一缺陷即上条 DRR 队列指针（已修） |
| 批次 3 TUN（D8/D19/D20） | 完整。`tunLifecycleState`+`lifecycleMu`、`WriteLoopCloseCh` 稳定通道、`dnsIface`、`TeardownForce`；Windows `removeAdapterAPI`(FriendlyName)/`deleteResidualRoutesAPI(luid)`/`deleteTUNAdapter`/`saveWindowsTunState`↔`loadWindowsTunState`；Linux `restorePersistedLinuxDNS`、`routeProtocolPhaethon=200`；macOS `saveDarwinDNSBackup`/`restoreDarwinDNSBackup`、`saveForeignSplits`/`restoreForeignSplits`；三平台 `CleanupResidual` 均已接入恢复 |
| 批次 4 健康检查与拨号 | **不完整（只有骨架）**：`mesh.GossipInfo.MinProtocolVersion` 字段与 `protocolCompatible`/`negotiatedProtocolVersion` 已加，但**全仓没有任何地方给 `MinProtocolVersion` 赋值**（所有 `GossipInfo` 构造点只设 `ProtocolVersion`），`Peer.negotiatedVersion` 只写不读 → D25 实际不生效。订阅并发、SSH 池、配置/运行态分离尚未动 |

## 7. 教训与后续约束

1. **不要整篇 Read 大文件**（本项目 `gvisor-fork/pkg/buffer/*.go`、`tun/route_*` 动辄上千行）：用 `Grep` 定位 + `Read` 带 `offset/limit`。
2. 长会话要**分批提交**，每批完成即 commit（见 §0 铁律）；本次三批改动全部未提交，一次 rewind 归零。
3. `--resume` 前若看到 `compacted: false` / `context length has exceeded`，应立即开新会话 + 文档交接，不要继续在同一会话里加内容。
4. 会话缓存目录 `%LOCALAPPDATA%\trae-cli\sessions\4f40e2d3-...\` 仍占 657 MB（`events.jsonl` 637 MB + `traces.jsonl` 17 MB）。**在恢复成果提交之前不要删**——它是唯一的历史来源。
5. 新会话开工建议顺序：先提交恢复成果 → 做批次 1 的部署验证（阶段 6.3）→ 再补完批次 4。
