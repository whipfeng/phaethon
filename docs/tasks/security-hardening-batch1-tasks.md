# 安全止血批次（Batch 1）任务

> 创建日期: 2026-11-09
> 关联方案: [security-hardening-batch1.md](../plans/security-hardening-batch1.md)
> 关联审计: [full-code-audit-20261109.md](../issues/full-code-audit-20261109.md)
> 状态: 代码完成，待部署验证（2026-11-09）

## 阶段 1: 包分发改为手动升级（D3）

### Task 1.1: 目录与 pin
- [x] 新增 `data/packages-pending/` 常量与目录创建
- [x] 新增 `data/state/worker-pin` 读写（版本号）
- [x] watchdog `selectWorkerBinary` 优先读 pin，只扫 `data/packages/`

### Task 1.2: 接收与上传
- [x] `apiPackageReceive` 只落 pending、`Published=false`
- [x] 删除 `go checkForNewerVersion` 自动升级（改为仅记录日志）与 `go DistributePackage`
- [x] receive/upload 加 `http.MaxBytesReader`

### Task 1.3: 启用版本 API + UI
- [x] 新增激活语义：`POST /api/packages/{id}/activate?restart=now|later`（复用 publish 路径）
- [x] 激活时移动 pending → packages、写 pin；`restart=now` 走 `os.Exit(0)` 由 watchdog 拉起
- [x] admin UI：状态徽标（已启用/待启用）+ 启用按钮 + 重启确认

### Task 1.4: 自动升级移除
- [x] `checkForNewerVersion` 不再 `os.Exit`，仅记录“新版本可用”

## 阶段 2: 数据库接口默认关闭（D4）

### Task 2.1: 开关
- [x] `--enable-debug-api` flag + `PHAETHON_DEBUG_API=1` 环境变量
- [x] AdminServer 增加 `debugAPI` 开关与 Setter
- [x] 关闭时 `/api/db/**`、pprof、stall-dump 返回 404

### Task 2.2: 只读与白名单
- [x] 仅允许 GET；POST/DELETE 返回 403
- [x] bucket 白名单（proxies/proxy-groups/.../webhooks）
- [x] 列表返回条数上限（1000）

## 阶段 3: 监听地址与注册端口（D12/D13）

### Task 3.1: mapping listen_addr
- [x] `config.Mapping` 增加 `listen-addr`，空值等价 `0.0.0.0`
- [x] `server.startTCP` / `startTLS` 使用该地址
- [x] 所有内置协议监听接入

### Task 3.2: 空密码告警
- [x] socks5 密码为空启动时 WARN

### Task 3.3: 注册端口
- [x] 未配置范围时默认 `20000-39999`；`0,0` 不再表示不限制
- [x] 拒绝 < 1024（含 PreferredPort）
- [ ] 动态 listener 监听地址跟随全局配置（当前仍为 0.0.0.0，待后续统一入口）

## 阶段 4: 后台会话与凭据（D23）

### Task 4.1: 密码哈希
- [x] 引入 bcrypt；登录改哈希校验（明文旧密码拒绝，需管理员重置）
- [x] setup / 配置修改时哈希存储

### Task 4.2: 会话密钥与 Cookie
- [x] 会话签名只用独立 `sessionSecret`，不再复用 admin token
- [x] Cookie 增加 `Secure`（TLS 时）、有效期缩短为 24h
- [x] 改密时轮换会话密钥，旧会话立即失效

### Task 4.3: 接口脱敏
- [x] `GET /api/config/admin` 不返回密码/token/私钥，仅返回 `Has*` 标记

## 阶段 5: 顺带修复

### Task 5.1: trojan 日志脱敏
- [x] 认证失败只打印指纹前缀，不打印完整口令哈希

### Task 5.2: setup 页面数据格式
- [x] `admin/templates/setup.html` 兼容数组与 `{proxies:[]}` 两种响应

## 阶段 6: 验证与归档

### Task 6.1: 构建与测试
- [x] `go build .` 通过
- [x] `GOOS=linux` / `GOOS=darwin` 交叉编译通过
- [x] `go vet .` 通过
- [x] `go test ./admin/... ./db/... ./server/...` 通过

### Task 6.2: 文档
- [x] 更新 `docs/index.md`
- [x] 审计文档对应 D 项标记“已实施”

### Task 6.3: 部署验证（待执行）
- [ ] VM：上传包 → 确认只进 pending、不触发升级
- [ ] VM：激活版本（下次重启生效 / 立即重启）→ watchdog 使用 pin
- [ ] VM：未开 `--enable-debug-api` 时 `/api/db` 返回 404；开启后只读 + 白名单
- [ ] VM：映射配置 `listen-addr: 127.0.0.1` 后只监听回环
- [ ] VM：重新设置管理员密码（bcrypt），旧会话失效
