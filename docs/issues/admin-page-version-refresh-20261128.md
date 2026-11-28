# 控制台页面版本通知接入不一致（2026-11-28）

**创建时间**: 2026-11-28
**状态**: 已修复，vm（v0.8.3-128-ga831d38）验证通过，待扩散
**优先级**: 中（页面可信度；数据本身正确）

## 现象

- 映射页新建映射后列表不自动刷新，需要手动切页/刷新。
- 类似疑点：管理页（SSH 公钥 / API 密钥）、套餐页在跨端变更后不刷新，
  或反复进出页面后刷新次数异常。

## 根因（五个独立缺陷）

1. **映射页服务端模板与旧 JS 渲染器漂移**
   `app.js` 的 `fetchMappings()` / `updateMappings()` 查找 `#mapping-table`，
   但服务端模板早已不再输出该 id，`querySelector` 返回 `null` 后静默 return。
   版本号通知本身是正常到达的，只是重绘动作什么也没做。
2. **topic 键校验写错**
   `pollReverseList` 比对 `targetVersions.reverse`（实际 topic 为 `reverse-configs`），
   `fetchTUNStatus` 比对 `targetVersions.tun`（存储配置 topic 为 `tun-config`），
   版本一致性检查恒不成立或误判。
3. **页面级订阅缺少生命周期**
   `admin.html` / `package.html` 的 inline 脚本用全局 `onBusinessVersion` 注册，
   HTMX 切页后 handler 仍留在服务里，反复进出会累积重复请求；
   `package.html` 还自建了一个私有 `EventSource('./api/events')`，
   每次进入都多一条 SSE 连接，且与全局连接互不知情。
4. **遮罩关闭只在首屏绑定**
   `app.js` 在 DOMContentLoaded 时对当时的 `.modal` 节点逐个 `addEventListener`。
   HTMX 换页/刷新会替换节点，之后新模态框没有遮罩关闭监听：
   点遮罩既关不掉弹窗，也不会触发 `flushPendingPageRefreshes()`，
   挂起的刷新被永久滞留。改为 document 级事件委托。
5. **片段内联脚本顶层 `let/const` 重复声明**
   HTMX 在同一个 document 内重复执行片段脚本，顶层 `let/const` 属于全局词法作用域，
   第二次进入同一页面即抛 `SyntaxError: Identifier 'x' has already been declared`，
   整段脚本不再执行（`resolvers.html` 的 `meshNodeId`、
   `proxies.html` 的 `_p2pPopName`）。改为 `var` 后重入安全。

## 修复

- 映射页与解析器页改为 **SSR 整段刷新**：版本变化时 `reloadPage()`
  （htmx 局部重取当前 URL 到 `#main-content`），服务端模板是表格与统计卡片的唯一来源，
  不再维护第二套 JS 渲染器（已删除 `fetchMappings`/`updateMappings`）。
- 新增 `onPageBusinessVersion(topic, handler, label)`：返回的退订函数进入
  `pageScopedSubscriptions` 队列，在 `htmx:beforeSwap`（target=`#main-content`）
  时统一退订，页面 DOM 与订阅同生命周期。`admin.html` / `package.html` 改用它。
- `package.html` 删除私有 EventSource，改订阅全局 `packages` topic。
- 修正 `reverse-configs` / `tun-config` 的版本键；`tun` 运行时 topic 单独驱动
  TUN 状态重取（页面卡片与 dashboard 摘要都覆盖）。
- 弹窗保护：用户在编辑映射/解析器时版本变化只记 pending，
  关闭弹窗（按钮 / 遮罩 / Esc）后立即补刷，避免冲掉正在编辑的表单。
- 首次快照（SSR 刚渲染完）不触发多余重载：`ssrPageVersions` 记录首版。

## 验证

- `go build ./...`、`go test ./util/ ./admin/`、`node --check admin/static/app.js` 通过。
- vm 环境（Playwright，带客户端证书，v0.8.3-128-ga831d38）：
  - 映射页：弹窗新建后自动出现；跨端新建在弹窗打开时保持不动；
    点 Cancel / 点遮罩 / 按 Esc 关闭后补刷；空闲跨端变更只刷新一次。
  - 解析器页：空库首次新建即刷新；外部启用开关/删除各触发恰好一次刷新；
    空闲 8s 无刷新循环。
  - Admin 页持有 3 个页面级订阅（ssh / ssh_keys / api-keys），离开后归零；
    套餐页反复进出无 handler 累积；全程只有 1 条 `/api/events` SSE 连接。
  - 每个导航页连续进入两次无 JS 错误（覆盖上述第 5 条缺陷的回归守卫）。

## 备注

- 不再为每个页面手写增量渲染：新页面优先复用 SSR + `refreshSSRPage`，
  避免第二次出现“模板列数/单元格 id 漂移”。
