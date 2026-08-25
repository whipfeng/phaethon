# 项目提取与配置重构设计 (Project Extraction & Config Refactor Design)

> 版本: 1.0.0
> 日期: 2026-08-25
> 状态: ACTIVE
> 负责人: Phaethon Dev
> 依赖: docs/specs/config_spec.md v1.0.0

## 1. 背景

原 `whipfeng/phaethon` 是混合仓库中的一部分，与 Java 版 phaethon 项目共存。为了独立演进、单独版本控制，将 Go 实现提取为独立仓库 `phaethon`。

## 2. 目标

1. 从 `whipfeng/phaethon` 提取完整 Go 源码到新目录 `phaethon`
2. 模块名从 `phaethon-go` 改为 `phaethon`
3. 配置文件改造为 `.env + config.yaml`
4. 配置从工作目录加载，不再依赖 `CONF_PATH`
5. 建立干净的 `.gitignore`
6. 保留编译能力

## 3. 目录结构

```
phaethon/
├── admin/              # 管理面板
├── cmd/                # 子命令
├── config/             # 配置解析
├── conf/               # 内嵌默认配置
│   └── default.yaml
├── dialer/             # 出站协议
├── docs/               # MDD 文档
├── reverse/            # 反向连接
├── server/             # 入站协议
├── tools/              # 工具脚本
├── tun/                # TUN 模式
├── util/               # 工具函数
├── main.go             # 入口
├── main_tun.go         # TUN 入口
├── main_tun_nows.go    # Windows no-ws 入口
├── main_tun_windows.go # Windows TUN 入口
├── go.mod
├── go.sum
├── Makefile
├── Dockerfile.proxy
├── README.md
└── .gitignore
```

## 4. 关键设计决策

### 4.1 模块名

Go module path 从 `phaethon-go` 改为 `phaethon`，所有内部 import 同步替换。

### 4.2 配置加载

- 工作目录 = `os.Getwd()`
- 先加载 `.env`，再加载 `config.yaml`
- `config.yaml` 经过环境变量替换后再解析 YAML
- 内嵌默认配置保留在 `conf/default.yaml`，仅作为 fallback

### 4.3 移除的特性

- `CONF_PATH` 环境变量
- `rule.yaml` 命名
- `rule-{env}.yaml` 环境覆盖
- base + env merge 逻辑

### 4.4 .env 解析器

自研轻量 parser，不引入 `godotenv` 依赖：
- 支持基本 key=value
- 支持单/双引号
- 支持 export 前缀
- 支持注释和空行

## 5. 风险与缓解

| 风险 | 缓解 |
|------|------|
| import 路径替换遗漏 | 全局搜索 `phaethon-go/` 并替换 |
| 默认配置路径引用错误 | 保留 `conf/default.yaml` 内嵌路径 |
| `.env` 解析不兼容 | 提供清晰文档和示例 |

## 6. 变更记录

| 版本 | 日期 | 变更内容 | 作者 |
|------|------|----------|------|
| 1.0.0 | 2026-08-25 | 初始版本 | Phaethon Dev |
