# 提取与配置重构任务 (Extract & Config Refactor Tasks)

> 分支: extract-init
> 日期: 2026-08-25
> 依赖: docs/plans/extract_design.md v1.0.0

## 任务列表

- [x] Task 1: 提取源码到新目录 `phaethon`
  - 复制 `whipfeng/phaethon` 的源码目录和必要文件
  - 排除构建产物、二进制、日志、测试配置目录
  - 验收: 新目录包含完整可编译源码

- [x] Task 2: 更新模块名与 import 路径
  - `go.mod` module 从 `phaethon-go` 改为 `phaethon`
  - 所有 `.go` 文件中 `"phaethon-go/` 替换为 `"phaethon/`
  - `build.ps1` / `Makefile` / `Dockerfile.proxy` 中二进制名改为 `phaethon`
  - 验收: `grep -r '"phaethon-go/'` 无结果

- [x] Task 3: 实现 `.env` 解析器
  - 在 `config/env.go` 中实现 `.env` 文件解析
  - 实现 `${VAR}` / `$VAR` / `$${VAR}` / `${VAR:-default}` 替换
  - 验收: 单元测试覆盖基本场景

- [x] Task 4: 改造配置加载逻辑
  - `main.go` 中从工作目录加载 `config.yaml` 和 `.env`
  - 移除 `CONF_PATH` 和 `rule-{env}.yaml` 逻辑
  - 重命名 `rule.yaml` 为 `config.yaml`
  - 验收: 程序能正确读取 `.env` 替换后的配置

- [x] Task 5: 建立 `.gitignore`
  - 忽略构建产物、二进制、日志、.env、.phaethon 等
  - 验收: `git status` 仅显示应跟踪文件

- [x] Task 6: 初始化 git 仓库并验证编译
  - `git init`
  - `go build` 通过
  - 验收: 生成 `phaethon` / `phaethon.exe` 可执行文件

## 提交计划

每个 Task 完成后独立提交。

## 备注

- Admin 面板仍保留 `envConf` / `envPath` / `saveTarget` 字段，但已不会触发，属于遗留字段，后续可清理。
- `config.yaml` 中的 `${VAR}` 占位符在 Admin 面板保存后会被解析值替换，这是已知限制。
