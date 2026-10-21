# mTLS 双向认证工具

本工具用于管理 Phaethon 管理员面板的双向 TLS（mTLS）认证，替代传统的用户名/密码登录。

## 什么是 mTLS？

mTLS（Mutual TLS）是双向 TLS 认证：
- **传统 TLS**：客户端验证服务器证书（HTTPS）
- **mTLS**：客户端验证服务器证书 + 服务器验证客户端证书

**优势：**
- ✅ 无需用户名和密码
- ✅ 登录页面完全隐藏
- ✅ 基于证书的安全认证
- ✅ 防止未授权访问
- ✅ 适合自动化脚本和 CI/CD

## 功能

### 1. 证书管理 (`phaethon-cert`)

生成 CA 和客户端证书用于 mTLS 认证。

**命令：**

```bash
# 生成 CA 证书（只需一次）
phaethon-cert ca

# 生成客户端证书（可以生成多个）
phaethon-cert client <ca-key> <ca-cert>
```

**示例：**

```bash
# 1. 生成 CA 证书
./phaethon-cert ca
# 输出：ca.key（CA 私钥）和 ca.crt（CA 证书）

# 2. 生成客户端证书
./phaethon-cert client ca.key ca.crt
# 输出：client.key（客户端私钥）和 client.crt（客户端证书）
```

### 2. 数据库管理 (`phaethon-db`)

管理嵌入式数据库中的管理员配置，包括设置 CA 证书。

**命令：**

```bash
# 获取管理员配置
phaethon-db get-admin

# 设置客户端 CA 证书（启用 mTLS）
phaethon-db set-client-ca <ca-cert-file>

# 清除客户端 CA 证书（禁用 mTLS）
phaethon-db clear-client-ca
```

**示例：**

```bash
# 启用 mTLS
phaethon-db set-client-ca ca.crt

# 禁用 mTLS
phaethon-db clear-client-ca
```

## 完整部署流程

### 步骤 1：生成证书

```bash
# 在本地开发机器上
cd tools
go build -o ../dist/phaethon-cert phaethon-cert.go
cd ..

# 生成 CA 证书
./dist/phaethon-cert ca

# 生成客户端证书
./dist/phaethon-cert client ca.key ca.crt
```

**生成的文件：**
- `ca.key` - CA 私钥（保密，不要泄露）
- `ca.crt` - CA 证书（配置到服务器）
- `client.key` - 客户端私钥（安装到浏览器）
- `client.crt` - 客户端证书（安装到浏览器）

### 步骤 2：配置服务器

**方法 A：使用数据库工具（推荐）**

```bash
# 在运行 phaethon 的机器上
cd /path/to/phaethon

# 设置 CA 证书
./phaethon-db set-client-ca ca.crt

# 重启服务
./phaethon restart
```

**方法 B：手动编辑配置**

如果配置文件还存在（旧版本），可以添加：

```yaml
admin:
  enabled: true
  addr: ":39999"
  tls-cert: "/path/to/server.crt"
  tls-key: "/path/to/server.key"
  client-ca: |
    -----BEGIN CERTIFICATE-----
    MIIC...（ca.crt 的内容）...
    -----END CERTIFICATE-----
```

**注意：** 配置已迁移到数据库，直接编辑 config.yaml 无效！

### 步骤 3：安装客户端证书到浏览器

**方法 A：转换为 PKCS#12 格式（推荐）**

```bash
# 转换为 PKCS#12（需要设置密码）
openssl pkcs12 -export \
  -out client.p12 \
  -inkey client.key \
  -in client.crt

# 输入密码（例如：changeme）
```

**导入到浏览器：**

- **Chrome/Edge**：
  1. 设置 → 隐私和安全 → 安全 → 管理证书
  2. 导入 → 选择 `client.p12` → 输入密码

- **Firefox**：
  1. 设置 → 隐私与安全 → 安全 → 证书
  2. 查看证书 → 您的证书 → 导入
  3. 选择 `client.p12` → 输入密码

- **Safari**：
  1. 钥匙串访问 → 文件 → 导入项目
  2. 选择 `client.p12` → 输入密码

**方法 B：直接导入 PEM 格式**

某些浏览器支持直接导入 `client.key` 和 `client.crt`，但 PKCS#12 更通用。

### 步骤 4：访问管理员面板

```bash
# 使用浏览器访问
https://vm.phn

# 浏览器会提示选择客户端证书
# 选择刚导入的证书
# 自动登录，无需用户名密码！
```

**使用 curl 测试：**

```bash
# 使用客户端证书访问 API
curl -k \
  --cert client.crt \
  --key client.key \
  https://vm.phn/api/status

# 或使用 PKCS#12
curl -k \
  --cert client.p12:changeme \
  https://vm.phn/api/status
```

## 认证优先级

管理员认证按以下顺序检查：

1. **mTLS 客户端证书**（最高优先级）
2. **Session 认证**（已登录的浏览器会话）
3. **签名认证**（X-Signature + X-Timestamp 头）
4. **Token 认证**（Authorization: Bearer <token> 头）

**当 mTLS 启用时：**
- 登录页面仍然可以访问，但 mTLS 客户端会跳过登录
- 没有客户端证书的访问会被要求登录（如果配置了账密）
- 可以同时启用 mTLS 和账密，实现多因素认证

## 使用场景

### 场景 1：完全隐藏登录页面

只允许 mTLS 认证，禁用账密：

```bash
# 设置 CA 证书
./phaethon-db set-client-ca ca.crt

# 清除账密（可选）
./phaethon-db set-password ""
```

**效果：**
- 没有客户端证书的访问会收到 403 Forbidden
- 登录页面无法使用（因为没有账密）
- 只有持有有效客户端证书的浏览器可以访问

### 场景 2：自动化脚本

在 CI/CD 流水线中使用客户端证书：

```bash
#!/bin/bash
# CI/CD 脚本

# 使用客户端证书访问 API
curl -k \
  --cert client.crt \
  --key client.key \
  -X POST \
  -F "package=@app.tar.gz" \
  https://vm.phn/api/packages

# 或使用 PKCS#12
curl -k \
  --cert client.p12:changeme \
  -X POST \
  -F "package=@app.tar.gz" \
  https://vm.phn/api/packages
```

### 场景 3：多客户端管理

为不同的管理员或设备生成不同的客户端证书：

```bash
# 为管理员 A 生成证书
./phaethon-cert client ca.key ca.crt
mv client.key admin-a.key
mv client.crt admin-a.crt

# 为管理员 B 生成证书
./phaethon-cert client ca.key ca.crt
mv client.key admin-b.key
mv client.crt admin-b.crt

# 为 CI/CD 生成证书
./phaethon-cert client ca.key ca.crt
mv client.key ci-cd.key
mv client.crt ci-cd.crt
```

**优势：**
- 可以撤销单个证书（需要重新生成 CA）
- 可以追踪哪个证书被使用（通过证书主题）
- 可以为不同设备生成不同证书

## 编译工具

```bash
# 编译证书工具
go build -o dist/phaethon-cert tools/phaethon-cert.go

# 编译数据库工具
go build -o dist/phaethon-db tools/phaethon-db.go
```

## 文件说明

- `ca.key` - CA 私钥（保密，不要泄露！）
- `ca.crt` - CA 证书（配置到服务器）
- `client.key` - 客户端私钥（安装到浏览器）
- `client.crt` - 客户端证书（安装到浏览器）
- `client.p12` - PKCS#12 格式（可选，方便导入浏览器）

## 注意事项

1. **CA 私钥安全**：`ca.key` 必须妥善保管，不要提交到版本控制
2. **证书有效期**：
   - CA 证书：10 年
   - 客户端证书：5 年
3. **证书撤销**：如果需要撤销证书，必须重新生成 CA 并重新分发客户端证书
4. **浏览器兼容性**：所有现代浏览器都支持 mTLS
5. **服务器证书**：mTLS 需要服务器已经配置了 TLS 证书（tls-cert + tls-key）
6. **备份**：建议备份 CA 证书和私钥，丢失后无法恢复

## 故障排查

### 问题：浏览器提示"需要客户端证书"

**原因：** 服务器启用了 mTLS，但浏览器没有安装客户端证书

**解决：**
1. 生成客户端证书：`./phaethon-cert client ca.key ca.crt`
2. 转换为 PKCS#12：`openssl pkcs12 -export -out client.p12 -inkey client.key -in client.crt`
3. 导入到浏览器

### 问题：证书导入后仍然无法访问

**检查项：**
1. 服务器是否正确配置了 CA 证书
2. 客户端证书是否由该 CA 签发
3. 证书是否过期
4. 浏览器是否选择了正确的证书

**调试方法：**

```bash
# 检查服务器配置
./phaethon-db get-admin

# 验证证书链
openssl verify -CAfile ca.crt client.crt

# 查看证书详情
openssl x509 -in client.crt -text -noout
```

### 问题：curl 报证书错误

**检查项：**
1. 证书路径是否正确
2. 证书格式是否正确（PEM 或 PKCS#12）
3. 密码是否正确（PKCS#12）

**解决方法：**

```bash
# 使用 PEM 格式
curl -k --cert client.crt --key client.key https://vm.phn/api/status

# 使用 PKCS#12 格式
curl -k --cert client.p12:password https://vm.phn/api/status
```

## 技术细节

### 证书结构

**CA 证书：**
- 类型：X.509 v3
- 密钥：ECDSA P-256
- 用途：证书签名（KeyUsageCertSign）
- 有效期：10 年
- IsCA：true

**客户端证书：**
- 类型：X.509 v3
- 密钥：ECDSA P-256
- 用途：客户端认证（ExtKeyUsageClientAuth）
- 有效期：5 年
- IsCA：false
- 签名者：CA 证书

### TLS 握手流程

1. 客户端发送 ClientHello
2. 服务器发送 ServerHello + 服务器证书 + **CertificateRequest**（请求客户端证书）
3. 客户端发送客户端证书 + CertificateVerify（签名证明拥有私钥）
4. 服务器验证客户端证书（使用 CA 证书）
5. 验证通过 → 建立连接
6. 验证失败 → 连接中断

### 代码位置

- TLS 配置：`admin/admin.go:774-802`
- 认证中间件：`admin/admin.go:1023-1077`
- 配置结构：`config/config.go:1286-1301`
- 证书工具：`tools/phaethon-cert.go`
- 数据库工具：`tools/phaethon-db.go`

## 安全建议

1. **CA 私钥保护**：
   - 存储在安全的地方（HSM 或加密的 USB）
   - 不要提交到版本控制
   - 限制访问权限

2. **客户端证书管理**：
   - 为每个用户/设备生成独立的证书
   - 记录证书分发情况
   - 定期审查证书使用情况

3. **证书轮换**：
   - 建议每 1-2 年轮换客户端证书
   - CA 证书可以长期使用（10 年）

4. **备份策略**：
   - 备份 CA 证书和私钥
   - 备份所有客户端证书
   - 测试恢复流程

5. **监控和审计**：
   - 记录证书使用情况
   - 监控异常的证书请求
   - 定期审查访问日志
