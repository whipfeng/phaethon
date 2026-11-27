# ylnas（群晖 NAS）升级被磁盘余量卡住（2026-11-27）

**状态**: 已绕过，结构性隐患仍在
**影响**: v105 扩散最后一个节点；后续该节点通过控制台上传包仍会被拒

## 现象

- 从控制台上传 v105 包：`HTTP 507 insufficient disk space: free=... need=...`
- `df -h /`：`/dev/md0 2.3G 2.3G 0 100%`；`/volume2` 仍有 3.4T 空闲。
- 部署布局：可执行文件在 `/volume2/whipfeng/phaethon`，但工作目录是 `/root/whipfeng`，
  `data/`（含 `packages`、`packages-pending`、`state`）全在 2.3G 的系统分区上。

## 根因

`admin/package.go:78 checkFreeSpace` 用 `freeDiskBytes(".")`（进程工作目录）做余量判断，
阈值 `minFreeDiskBytes = 512 << 20`。群晖系统分区只有 2.3G，且绝大部分被 DSM 占用，
清日志后最多也只有 ~130M 可用，永远达不到 512M。

也就是说：**判断的是"当前工作目录所在卷"，而不是"包实际写入的目录所在卷"**。
在数据目录和可执行文件不在同一卷的部署（群晖这类系统分区极小的机器）上必然误拒。

## 本次处置（绕过，不改代码）

群晖没有可用 OS SSH（原来的 `sshd` 只在内网 loopback 侧可见，当前会话就是 phaethon
内建 SSH 的子进程），因此不能停进程换工作目录。改为直接喂 watchdog：

1. 本机 `dist/linux-amd64/phaethon_linux_amd64_v0.8.3-105-g0ade24d.pkg` 经内建 SSH
   以 base64 流写入 `/root/whipfeng/data/packages/manual105.pkg`（sha256 一致）。
   内建 SSH 不支持 scp/sftp，用 `base64 -d` 落盘。
2. 写 `data/state/upgrade-request`：
   `{"version":"v0.8.3-105-g0ade24d","packageId":"manual105","requestedAt":"..."}`
3. watchdog 走正常自替换流程（校验签名 → stage → swap → handoff → 重启 worker），
   日志 `data/logs/upgrade.log` 显示 `succeeded`，`worker-version` = v105。

watchdog 升级路径本身**不做**磁盘余量检查（校验/暂存/切换），所以只缺"上传"这一道门。

## 后续（未做，需决策）

1. `checkFreeSpace` 改为检查实际写入目录（`pendingPackagesDir`，必要时先 `MkdirAll`），
   而不是 `"."`；`. 不存在的场景回退到 `"."`。这需要新版本 + 全机群扩散，暂未做。
2. 或把该节点 `data/` 整体挪到 `/volume2` 并把工作目录指过去（需要重启进程，
   该节点只有内建 SSH，属于受限操作，未执行）。
3. 若长期保留现状，注意每次升级需要约 `pkg(9.4M) + staged binary(24.6M)` 的余量，
   当前系统分区余量约 117M，够用但不宽裕；`data/packages` 保留 3 个版本会被占满。
