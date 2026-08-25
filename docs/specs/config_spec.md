# 配置系统规格 (Config System Specification)

> 版本: 1.0.0
> 日期: 2026-08-25
> 状态: ACTIVE
> 负责人: Phaethon Dev

## 1. 目标

定义 `phaethon` 的配置格式、加载顺序、环境变量替换规则。

## 2. 配置格式

- **主配置**: `config.yaml`，YAML 格式
- **环境变量文件**: `.env`，key=value 格式

## 3. 文件位置

配置文件与运行程序在同一工作目录下：

```
/workdir
  ├── phaethon        # 可执行文件
  ├── config.yaml   # 主配置
  └── .env          # 环境变量
```

程序通过 `os.Getwd()` 获取工作目录，并在此目录下查找 `config.yaml` 和 `.env`。

## 4. 加载顺序

1. 从工作目录加载 `.env`（如果存在）
2. 从工作目录加载 `config.yaml`
3. 如果 `config.yaml` 不存在，把内嵌的默认配置复制为 `config.yaml`，然后加载它
4. 对 `config.yaml` 内容做环境变量替换（`${VAR}` / `$VAR`）
5. 解析 YAML
6. 复制失败时，回退到内嵌的默认配置（`conf/default.yaml`）

> 设计意图：首次启动无需手动创建配置文件即可运行；默认配置不含任何凭证或基础设施地址，避免把密码编译进二进制。凭证和真实代理通过 `.env` 或启动后编辑 `config.yaml` 提供。

## 5. 环境变量替换规则

支持以下语法：

| 语法 | 说明 |
|------|------|
| `${VAR}` | 替换为环境变量 VAR 的值 |
| `$VAR` | 替换为环境变量 VAR 的值（仅支持字母数字下划线） |
| `$${VAR}` | 转义为 `${VAR}` 字符串 |
| `${VAR:-default}` | VAR 未定义或为空时使用 default |

环境变量来源优先级：
1. 系统环境变量
2. `.env` 文件中定义的值

## 6. .env 文件格式

```bash
# 注释
KEY=value
KEY2="value with spaces"
KEY3='single quoted'
export KEY4=value
```

解析规则：
- 忽略空行和以 `#` 开头的注释行
- 支持 `KEY=value`、`KEY="value"`、`KEY='value'`
- 支持 `export KEY=value`
- 引号内的值保留空格

## 7. 不再支持的特性

- `CONF_PATH` 环境变量（配置固定在工作目录）
- `rule.yaml` / `rule-{env}.yaml` 文件命名
- base + env 配置合并

多环境配置通过复制整个工作目录实现。

## 8. 变更记录

| 版本 | 日期 | 变更内容 | 作者 |
|------|------|----------|------|
| 1.0.0 | 2026-08-25 | 初始版本 | Phaethon Dev |
