# 公钥签名认证工具

本工具用于管理 Phaethon 嵌入式数据库（bbolt）和管理员签名认证。

## 功能

### 1. 数据库管理 (`phaethon-db`)

管理嵌入式 bbolt 数据库中的管理员配置。

**命令：**

```bash
# 获取管理员配置
phaethon-db get-admin

# 设置管理员密码
phaethon-db set-password <新密码>

# 设置公钥（用于签名认证）
phaethon-db set-public-key <公钥文件路径>
```

**示例：**

```bash
# 重置密码为 admin123
phaethon-db set-password admin123

# 设置公钥
phaethon-db set-public-key admin.pub
```

### 2. 签名认证 (`phaethon-sign`)

生成 ECDSA P-256 密钥对，为 HTTP 请求生成签名，用于无密码管理员认证。

**命令：**

```bash
# 生成密钥对
phaethon-sign keygen

# 签名 HTTP 请求
phaethon-sign sign <方法> <路径> <私钥文件> [请求体]
```

**示例：**

```bash
# 1. 生成密钥对
phaethon-sign keygen
# 输出：admin.key（私钥）和 admin.pub（公钥）

# 2. 将公钥添加到管理员配置
phaethon-db set-public-key admin.pub

# 3. 签名 GET 请求
phaethon-sign sign GET /api/status admin.key
# 输出：
# X-Timestamp: 1792584352
# X-Signature: 304502204d780158e7b39a6a...

# 4. 使用签名访问 API
curl -H "X-Timestamp: 1792584352" \
     -H "X-Signature: 304502204d780158e7b39a6a..." \
     http://vm.phn/api/status
```

## 签名认证原理

### 认证流程

1. **密钥生成**：使用 ECDSA P-256 算法生成公私钥对
2. **公钥配置**：将公钥添加到管理员配置的 `public-key` 字段
3. **请求签名**：使用私钥对请求进行签名
4. **服务端验证**：服务端使用公钥验证签名，通过则允许访问

### 签名格式

签名数据包含以下字段（每行一个）：

```
<时间戳>
<HTTP方法>
<URL路径>
<请求体SHA256哈希>
```

**示例：**

```
1792584352
GET
/api/status
e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
```

### 安全特性

- **防重放攻击**：签名包含时间戳，服务端验证时间窗口（±5分钟）
- **请求体完整性**：签名包含请求体的 SHA256 哈希
- **路径绑定**：签名绑定到特定的 URL 路径
- **方法绑定**：签名绑定到特定的 HTTP 方法

### 认证优先级

管理员认证按以下顺序检查：

1. **Session 认证**：已登录的浏览器会话
2. **签名认证**：X-Signature + X-Timestamp 头
3. **Token 认证**：Authorization: Bearer <token> 头

## 使用场景

### 场景 1：忘记密码

如果忘记了管理员密码，可以使用 `phaethon-db` 工具重置：

```bash
# 在运行 phaethon 的机器上
cd /path/to/phaethon
./phaethon-db set-password newpassword
```

### 场景 2：自动化脚本

使用签名认证可以在脚本中安全地访问管理员 API，无需在脚本中硬编码密码：

```bash
#!/bin/bash
# 签名私钥文件
KEY_FILE="admin.key"

# 签名请求
SIGN_OUTPUT=$(./phaethon-sign sign GET /api/status "$KEY_FILE")
TIMESTAMP=$(echo "$SIGN_OUTPUT" | grep "X-Timestamp:" | awk '{print $2}')
SIGNATURE=$(echo "$SIGN_OUTPUT" | grep "X-Signature:" | awk '{print $2}')

# 发送请求
curl -H "X-Timestamp: $TIMESTAMP" \
     -H "X-Signature: $SIGNATURE" \
     http://vm.phn/api/status
```

### 场景 3：CI/CD 集成

在 CI/CD 流水线中使用签名认证部署或管理 Phaethon：

```yaml
# .gitlab-ci.yml 示例
deploy:
  script:
    - ./phaethon-sign sign POST /api/packages admin.key < package.tar.gz > sig.txt
    - curl -H "X-Timestamp: $(grep Timestamp sig.txt | awk '{print $2}')" \
           -H "X-Signature: $(grep Signature sig.txt | awk '{print $2}')" \
           -F "package=@package.tar.gz" \
           http://vm.phn/api/packages
```

## 编译工具

```bash
# 编译数据库管理工具
go build -o dist/phaethon-db tools/phaethon-db.go

# 编译签名工具
go build -o dist/phaethon-sign tools/phaethon-sign.go
```

## 文件说明

- `admin.key` - ECDSA P-256 私钥（保密，不要泄露）
- `admin.pub` - ECDSA P-256 公钥（添加到管理员配置）
- `test-sign-auth.sh` - 测试脚本，演示如何使用签名认证

## 注意事项

1. **私钥安全**：私钥文件 `admin.key` 必须妥善保管，不要提交到版本控制
2. **时间同步**：签名验证依赖时间戳，确保客户端和服务端时间同步（NTP）
3. **公钥格式**：公钥必须是 PEM 编码的 PKIX 格式
4. **签名有效期**：签名有效期为 ±5 分钟，超时会被拒绝
5. **请求体限制**：对于大请求体，签名计算可能较慢

## 故障排查

### 问题：签名验证失败

**检查项：**

1. 公钥是否正确配置到管理员配置
2. 客户端和服务端时间是否同步
3. 签名是否过期（超过 5 分钟）
4. 请求路径和方法是否与签名时一致
5. 请求体是否与签名时一致

**调试方法：**

```bash
# 查看签名数据
./phaethon-sign sign GET /api/status admin.key
# 对比服务端日志中的签名数据

# 检查时间戳
date +%s  # 当前时间戳
# 与签名中的时间戳对比，差异不应超过 300 秒
```

### 问题：数据库工具无法运行

**检查项：**

1. 是否在 phaethon 工作目录运行
2. 数据库文件是否存在（data/phaethon.db）
3. phaethon 是否正在运行（数据库被锁定）

**解决方法：**

```bash
# 停止 phaethon 后再运行数据库工具
./phaethon stop
./phaethon-db set-password newpassword
./phaethon start
```

## 技术细节

### 签名算法

- **密钥类型**：ECDSA P-256（推荐）或 RSA
- **哈希算法**：SHA-256
- **签名格式**：ASN.1 DER（ECDSA）或 PKCS#1 v1.5（RSA）

### 数据库结构

管理员配置存储在 bbolt 数据库的 `config` bucket 中，key 为 `admin`。

**数据结构：**

```go
type AdminConfig struct {
    Enabled     bool   `yaml:"enabled"`
    Addr        string `yaml:"addr"`
    Token       string `yaml:"token"`
    AuthEnabled bool   `yaml:"auth-enabled"`
    Username    string `yaml:"username"`
    Password    string `yaml:"password"`
    PublicKey   string `yaml:"public-key,omitempty"` // PEM 编码的公钥
    // ... 其他字段
}
```

### 代码位置

- 签名验证：`admin/admin.go:verifySignature()`
- 认证中间件：`admin/admin.go:authMiddleware()`
- 配置结构：`config/config.go:AdminConfig`
- 数据库工具：`tools/phaethon-db.go`
- 签名工具：`tools/phaethon-sign.go`
