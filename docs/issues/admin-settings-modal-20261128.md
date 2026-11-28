# 控制台设置区块交互统一（2026-11-28）

**创建时间**: 2026-11-28
**状态**: 已修复，vm（v0.8.3-131-g71cb3ad）验证通过，待扩散
**优先级**: 中（交互一致性）

## 需求

1. 设置类区块只做展示，编辑一律走弹窗；不再在区块内直接编辑保存。
2. 危险操作（重启进程、关闭认证等）统一用确认弹框，去除密码二次确认。
3. 认证设置只保留一处入口（管理页），仪表盘安全卡片仅展示状态 + 跳转。
4. 密码输入只填一次，提供小眼睛切换显示/遮挡，风格与映射/代理页一致。

## 修复内容

- `admin.html`：Admin 设置 / 端口范围 / SSH 服务 / API 密钥四个区块改为只读展示 +
  弹窗编辑；Admin 弹窗密码字段单输入框 + 小眼睛（复用全局 `toggleFieldVisibility`）。
- `mesh.html`：Node Identity、TCP Keepalive 改为只读展示 + 弹窗编辑。
- `dashboard.html`：删除独立 `security-modal`（含重复的确认密码输入），安全卡片改为
  状态展示 + “前往管理页”跳转。
- `layout.html`：删除 `password-modal` 片段。
- `app.js`：删除 `showPasswordConfirm` / `openPasswordModal` 死代码；`restartProcess`
  去掉原生 `prompt()` 和 `X-Password-Confirm` 头；`toggleFieldVisibility` 提升为全局。
- `package.html`：两处原生 `confirm()` 改为统一 `openConfirmModal`。
- `proxies.html` / `mappings.html`：删除重复的 `toggleFieldVisibility` 本地定义，统一走 app.js。
- `i18n.js`：清理已无引用的 `raw.passwordConfirm`、`security.enable/change/modalTitle/
  changeTitle/enableTitle/confirmPassword`；修正 `admin.help` 中“需要密码确认”的过期文案。

## 附带修复：懒加载区块点编辑弹窗为空

Node Identity / TCP Keepalive 区块默认折叠，数据由 `data-collapse-load` 在首次展开时加载。
但“编辑”按钮在折叠状态下也可点击，此时内存中的 `currentMeshIdentity` /
`currentKeepalive` 尚未填充，弹窗显示空值或默认值（提交可能误改配置）。

修复：`openMeshIdentityModal` / `openTCPKeepaliveModal` 改为 async，首次打开前若未加载则
先 `await loadXxx()` 再回填表单。

## 验证

- `go build ./...`、`go test ./admin/...` 通过。
- vm（Playwright，客户端证书）：
  - 设置区块无可编辑输入；各弹窗初始隐藏，编辑打开后正确预填。
  - 密码默认遮挡，小眼睛可切换显示/遮挡。
  - 仪表盘无重复 security-modal，安全卡片跳转管理页。
  - `restartProcess` 走统一确认框，无原生 `prompt`。
  - 所有页面无残留原生 `confirm()` 内联调用。
  - 每个导航页连续进入两次无 JS 错误，`#confirm-modal` 不重复。
