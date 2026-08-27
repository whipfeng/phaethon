# Task 模板

## 任务信息

- 分支名：`branch-name`
- 目标：一句话概括本次开发目标
- 创建日期：YYYY-MM-DD
- 依赖计划：[plan_name.md](../plans/plan_name.md)

## 阶段 1: 文档与准备

### Task 1.1: 更新/创建规格
- [ ] 创建/更新 `docs/mdd/specs/spec_name.md`
- [ ] 创建本任务文件

## 阶段 2: 后端实现

### Task 2.1: 模块 A 改造
- [ ] 任务项一
- [ ] 任务项二

## 阶段 3: 前端实现

### Task 3.1: 页面调整
- [ ] 任务项一

## 阶段 4: 验证与收尾

### Task 4.1: 本地验证
- [ ] `go test ./...` 无回归
- [ ] `go build` 成功

### Task 4.2: 远程验证（如需要）
- [ ] 部署到测试环境
- [ ] 核心流程验证通过

### Task 4.3: 更新索引并收尾
- [ ] 更新 `docs/mdd/index.md`
- [ ] 标记所有 task 为 [x]
- [ ] commit 并 push
