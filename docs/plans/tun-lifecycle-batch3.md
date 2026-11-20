# TUN 生命周期与清理批次（Batch 3）设计

> 版本: 0.1.0
> 日期: 2026-11-09
> 状态: DRAFT
> 负责人: Phaethon Dev

## 变更记录

| 版本 | 日期 | 变更内容 | 作者 |
|------|------|----------|------|
| 0.1.0 | 2026-11-09 | 初始版本：由审计 D8/D19/D20 收敛 | Dev |

## 1. 目标

- **D8**：TUN 启停引入显式状态机与串行化；写入循环不再被一次性关闭信号杀死；DNS 启动/停止使用同一接口名；启动失败按步骤逆序回滚。
- **D19**：Windows 适配器删除按名称 + LUID 严格匹配；`StopTUN` 删除适配器；TUN 禁用/不可用也清理；weak-host/DNS 原值持久化。
- **D20**：Linux 路由/iptables 打标记后精确清理；只回滚真正修改过的 sysctl；DNS 三条路径统一备份恢复。macOS DNS 与抢占快照落盘。

## 2. 设计要点

### 2.0 运行期健康检查语义（2026-11-20 补定）

- `InterfaceExists(name) (bool, error)`：`error` 表示“无法判断”，调用方**不得**当作不存在；
  只有明确的 `false, nil` 才允许触发重建。历史缺陷是枚举失败返回 false，fd 耗尽时健康循环
  因此亲手拆掉了健康的 TUN（QG 2026-11-20，详见 `issues/qg-udp-fd-leak-20261120.md`）。
- Linux 存在性判定优先用 `stat("/sys/class/net/<name>")`，不占用 fd，fd 打满时仍然可靠；
  `net.Interfaces()` 仅作 sysfs 不可用时的兜底，并向上传播枚举错误。
- `readLoop` 非预期退出 → `readLoopDead` 置位，健康循环据此重建；正常 `StopTUN` 关闭
  通道退出不置位。
- Linux TUN fd 非阻塞打开（`O_NONBLOCK`），空闲时用 200ms `poll` 等待，使 `Close` 能及时
  唤醒读写循环；读写 syscall 由 RWMutex 保护，避免 close 后 fd 号复用被误读。

### 2.1 状态机（D8）

- 状态：`Idle / Starting / Running / Stopping`。
- 所有启停入口（启动、StopTUN、admin 热切换、启动失败回滚）串行化到同一控制路径；非法转换返回错误。
- `writeLoop` 的关闭信号改为稳定通道（仅 `StopStack` 关闭），TUN 停止期间由 `WriteLoopDevice` 是否为空决定行为，不再永久退出。
- 启动时记录实际注册 DNS 的接口名，停止时用同一个名字恢复。
- 每一步记录 undo，失败时逆序执行。

### 2.2 Windows（D19）

- `removeAdapterAPI`：按适配器名 + LUID 匹配，禁止“取第一个 WINTUN 实例”。
- 残留路由清理：带 LUID 过滤，禁止全表按 `0/0 + next-hop 0` 盲删。
- `StopTUN` 删除 `phaethontun` 适配器；TUN 禁用/不可用时执行一次带标记清理。
- weak-host / DNS 原值写入 `data/state/`，崩溃后可恢复。

### 2.3 Linux / macOS（D20）

- Linux 自建路由设协议号标记；iptables 使用专用链 + comment；清理只按标记匹配。
- 只回滚真正修改过的 sysctl（非 bypass 模式不得写 0）；排除路由崩溃后清理。
- DNS：`resolv.conf`、`resolvectl`、`nmcli` 统一备份/恢复，停止使用启动时记录的接口名。
- macOS：DNS 原值、`preemptForeignSplits` 快照落盘；保留抢占行为，只补崩溃恢复。

## 3. 验收

- 反复 admin 开关 TUN：不 panic、不黑洞、重建后双向可通。
- `kill -9` 后重启：DNS、路由、iptables、适配器恢复。
- TUN 禁用时残留被清理。
- 三平台编译通过；相关测试通过。
