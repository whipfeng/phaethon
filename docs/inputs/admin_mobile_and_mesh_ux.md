# Admin Console Mobile Adaptation & Mesh UX Improvement

## 背景

控制台 H5 目前存在两个问题：

1. **移动端适配不足**：在小屏幕设备上体验差，表格溢出、表单拥挤、弹窗显示不全
2. **Mesh 页面交互不一致**：路由配置和域名后缀路由使用文本输入框 + Save 按钮，与其他功能（如 Rules、Proxies）的标准增删改查模式不一致

## 问题详情

### 1. 移动端适配

当前状态：
- 只有基础的 `@media (max-width: 768px)` 规则
- 侧边栏折叠到 60px，隐藏文字只保留图标
- 表格没有横向滚动或卡片式布局
- 表单在小屏幕上拥挤
- 弹窗可能超出视口

期望改进：
- 表格支持横向滚动或改为卡片式布局
- 表单字段堆叠排列
- 弹窗改为全屏或底部抽屉
- 按钮和输入框大小适合触摸操作

### 2. Mesh 页面交互

当前状态：
- Domain Suffixes 和 Advertise 使用逗号分隔的文本输入框
- 只有一个 Save 按钮保存所有配置
- 没有单独的增删改查操作
- 与 Rules、Proxies 等页面的交互模式不一致

期望改进：
- 域名后缀路由改为表格展示 + 增删按钮
- 每条记录可以单独添加、编辑、删除
- 使用弹窗编辑单条记录（与 Rules 页面风格一致）
- Advertise CIDR 同样改为 CRUD 模式

## 影响范围

- `admin/static/style.css` - 移动端响应式样式
- `admin/templates/mesh.html` - Mesh 页面模板
- `admin/static/app.js` - Mesh 相关 JavaScript
- `admin/admin.go` - 可能需要添加 CRUD API

## 优先级

1. Mesh 页面交互重构（用户明确提到的痛点）
2. 移动端适配（整体改进）
