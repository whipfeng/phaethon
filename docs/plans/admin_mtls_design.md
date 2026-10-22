# Admin mTLS 认证设计文档

**创建时间**: 2026-10-22  
**状态**: 设计中  
**优先级**: 高

## 概述

为 Phaethon Admin 控制台添加 mTLS（双向 TLS）认证支持，同时保留原有的密码认证功能。用户可以在运行时选择使用哪种认证方式。

## 设计目标

1. **简化部署**：CA 证书预置到二进制中，无需额外配置
2. **灵活认证**：支持 mTLS 和密码认证两种方式，用户自己选择
3. **向后兼容**：保留原有的认证配置和功能
4. **安全可靠**：使用 ECDSA P-256 证书，符合现代安全标准

## 核心设计

### 认证模式

```
┌─────────────────────────────────────┐
│      用户访问 Admin 控制台           │
└────────────────┬────────────────────┘
                 │
                 ▼
        ┌────────────────┐
        │ auth-enabled?  │
        └────────┬───────┘
                 │
        ┌────────┴────────┐
        │                 │
       No                Yes
        │                 │
        ▼                 ▼
   直接放行        ┌──────────────┐
                   │ 有客户端证书？│
                   └──────┬───────┘
                          │
                   ┌──────┴──────┐
                   │             │
                  Yes            No
                   │             │
                   ▼             ▼
              mTLS 认证    重定向到登录页
                   │             │
                   ▼             ▼
              认证成功      密码认证
                              │
                              ▼
                         认证成功/失败
```

### CA 证书管理

**方案：预置到二进制**

```go
// admin/admin.go
import "embed"

//go:embed certs/ca.crt
var defaultClientCA []byte
```

**优点**：
- ✅ 零配置：部署即用，无需配置 CA 路径
- ✅ 安全性：CA 证书编译到二进制中，难以篡改
- ✅ 一致性：所有节点使用相同的 CA
- ✅ 简化部署：不需要分发 CA 文件

**缺点**：
- ❌ 更换 CA 需要重新编译二进制
- ❌ 所有节点必须使用同一个 CA

**结论**：对于当前场景（个人管理多个环境），预置 CA 是最佳选择。

### 认证流程

#### 1. mTLS 认证

```
客户端                              服务端
  │                                    │
  │--- ClientHello ------------------>│
  │                                    │
  │<-- ServerHello + 证书 -------------|
  │                                    │
  │<-- CertificateRequest -------------|  ← 包含预置的 CA 信息
  │                                    │
  │--- 客户端证书 -------------------->|  ← 浏览器自动选择匹配的证书
  │                                    │
  │<-- 验证通过 -----------------------|
  │                                    │
  │--- 请求资源 --------------------->|
  │                                    │
  │<-- 返回资源 -----------------------|
```

**关键点**：
- 服务端在 TLS 握手时发送 CertificateRequest
- 客户端根据 CA 信息在本地证书存储中匹配
- 如果找到匹配的证书，自动发送
- 服务端验证证书签名（使用预置的 CA）

#### 2. 密码认证

```
客户端                              服务端
  │                                    │
  │--- 访问页面 --------------------->│
  │                                    │
  │<-- 重定向到 /login ----------------|
  │                                    │
  │--- 提交用户名/密码 --------------->│
  │                                    │
  │<-- 验证通过，设置 Session ---------|
  │                                    │
  │--- 请求资源 --------------------->│
  │                                    │
  │<-- 返回资源 -----------------------|
```

### 配置结构

```yaml
admin:
  enabled: true
  addr: :39999
  
  # 是否开启认证
  auth-enabled: true  # false = 无需认证，true = 需要认证
  
  # 密码认证配置（auth-enabled=true 时使用）
  username: admin
  password: changeme
  
  # Token 认证（可选，用于 API 访问）
  token: your-secret-token
  
  # 以下字段不再需要（CA 预置到二进制）
  # client-ca: /path/to/ca.crt  ← 删除
```

### 认证中间件

```go
func (s *AdminServer) authMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        // 1. 公开路径无需认证
        if isPublicPath(r.URL.Path, r.Method) {
            next.ServeHTTP(w, r)
            return
        }

        // 2. 未开启认证，直接放行
        if !s.config.AuthEnabled {
            next.ServeHTTP(w, r)
            return
        }

        // 3. 尝试 mTLS 认证
        if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
            cert := r.TLS.PeerCertificates[0]
            util.LogModuleInfo("admin", "mTLS auth success: client=%s", 
                cert.Subject.CommonName)
            next.ServeHTTP(w, r)
            return
        }

        // 4. 尝试 Session 认证
        if user := s.verifySession(r); user != "" {
            next.ServeHTTP(w, r)
            return
        }

        // 5. 尝试 Token 认证
        token := r.URL.Query().Get("token")
        if token == "" {
            token = r.Header.Get("X-Admin-Token")
        }
        if token != "" && token == s.config.Token {
            next.ServeHTTP(w, r)
            return
        }

        // 6. 都没通过，重定向到登录页
        if !strings.HasPrefix(r.URL.Path, "/api/") {
            http.Redirect(w, r, "/login", http.StatusFound)
            return
        }
        w.WriteHeader(http.StatusUnauthorized)
        json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
    })
}
```

### TLS 配置

```go
func (s *AdminServer) Start() error {
    // ... 现有代码 ...
    
    // 使用预置的 CA 证书
    if len(defaultClientCA) > 0 {
        caCertPool := x509.NewCertPool()
        if !caCertPool.AppendCertsFromPEM(defaultClientCA) {
            return fmt.Errorf("failed to parse default CA certificate")
        }
        s.tlsConfig.ClientCAs = caCertPool
        
        if s.config.AuthEnabled {
            // 开启认证：可选 mTLS
            s.tlsConfig.ClientAuth = tls.VerifyClientCertIfGiven
            util.LogModuleInfo("admin", "authentication enabled (mTLS or password)")
        } else {
            // 未开启认证：不需要客户端证书
            s.tlsConfig.ClientAuth = tls.NoClientCert
            util.LogModuleInfo("admin", "authentication disabled")
        }
    }
    
    // ... 现有代码 ...
}
```

## 证书生成

### CA 证书

```bash
# 生成 CA 私钥
openssl ecparam -genkey -name prime256v1 -out ca.key

# 生成 CA 证书（10 年有效期）
openssl req -x509 -new -nodes -key ca.key -sha256 -days 3650 \
  -out ca.crt -subj "/O=Phaethon Admin/CN=Phaethon Admin CA"
```

### 服务器证书

```bash
# 生成服务器私钥
openssl ecparam -genkey -name prime256v1 -out server.key

# 生成 CSR
openssl req -new -key server.key -out server.csr \
  -subj "/CN=vm.phn"

# 使用 CA 签发（添加 SAN 扩展）
cat > server.ext << EOF
authorityKeyIdentifier=keyid,issuer
basicConstraints=CA:FALSE
keyUsage = digitalSignature, nonRepudiation, keyEncipherment, dataEncipherment
subjectAltName = @alt_names

[alt_names]
DNS.1 = vm.phn
DNS.2 = *.phn
IP.1 = 10.21.20.65
EOF

openssl x509 -req -in server.csr -CA ca.crt -CAkey ca.key \
  -CAcreateserial -out server.crt -days 365 -sha256 -extfile server.ext
```

### 客户端证书

```bash
# 生成客户端私钥
openssl ecparam -genkey -name prime256v1 -out client.key

# 生成 CSR
openssl req -new -key client.key -out client.csr \
  -subj "/CN=admin"

# 使用 CA 签发
cat > client.ext << EOF
authorityKeyIdentifier=keyid,issuer
basicConstraints=CA:FALSE
keyUsage = digitalSignature, nonRepudiation, keyEncipherment, dataEncipherment
extendedKeyUsage = clientAuth
EOF

openssl x509 -req -in client.csr -CA ca.crt -CAkey ca.key \
  -CAcreateserial -out client.crt -days 365 -sha256 -extfile client.ext

# 转换为 PKCS#12 格式（用于浏览器导入）
openssl pkcs12 -export -out client.p12 -inkey client.key \
  -in client.crt -certfile ca.crt -password pass:123456
```

## 部署流程

### 1. 编译二进制

```bash
# 将 CA 证书放到 admin/certs/ 目录
mkdir -p admin/certs
cp ca.crt admin/certs/

# 编译
make linux
make windows
```

### 2. 部署节点

```bash
# 上传二进制到节点
scp dist/linux-amd64/phaethon root@vm:/opt/phaethon/

# 配置数据库（通过 UI 或 API）
# 设置 auth-enabled: true
# 设置 username/password（如果需要密码认证）
```

### 3. 分发客户端证书

```bash
# 将 client.p12 分发给管理员
scp client.p12 admin@local:/path/to/

# 管理员导入到浏览器
# macOS: 双击 client.p12 → 导入到 Keychain
# Windows: 双击 client.p12 → 导入到证书存储
```

## 使用场景

### 场景 1：内网环境（无需认证）

```yaml
admin:
  auth-enabled: false
```

**效果**：任何人都可以直接访问，无需任何认证。

### 场景 2：使用 mTLS 认证（推荐）

```yaml
admin:
  auth-enabled: true
```

**效果**：
- 有客户端证书的用户 → 自动认证，无需登录
- 没有客户端证书的用户 → 跳转到登录页，使用密码认证

### 场景 3：API 访问

```bash
# 方式 1：使用 Token
curl https://vm.phn:39999/api/config?token=your-token

# 方式 2：使用证书
curl --cert client.crt --key client.key \
  https://vm.phn:39999/api/config

# 方式 3：使用 Session
curl -c cookies.txt -d "username=admin&password=xxx" \
  https://vm.phn:39999/login
curl -b cookies.txt https://vm.phn:39999/api/config
```

## 实现计划

### 阶段 1：预置 CA 证书

- [ ] 创建 `admin/certs/` 目录
- [ ] 将 `ca.crt` 复制到 `admin/certs/ca.crt`
- [ ] 修改 `admin/admin.go`，使用 `go:embed` 嵌入 CA
- [ ] 移除数据库中的 `ClientCA` 配置（可选）

### 阶段 2：实现认证选择

- [ ] 修改认证中间件，支持 mTLS 和密码认证
- [ ] 根据 `AuthEnabled` 配置决定是否强制认证
- [ ] 测试 mTLS 认证流程
- [ ] 测试密码认证流程

### 阶段 3：部署验证

- [ ] 编译新版本二进制
- [ ] 部署到 VM 环境
- [ ] 验证 mTLS 认证
- [ ] 验证密码认证
- [ ] 部署到其他环境（QG/JF/GG）

### 阶段 4：文档更新

- [ ] 更新用户文档
- [ ] 添加证书生成脚本
- [ ] 记录部署流程

## 安全考虑

### 1. CA 私钥保护

**风险**：CA 私钥泄露后，攻击者可以签发任意客户端证书。

**缓解措施**：
- CA 私钥离线存储（不放在服务器上）
- 使用强密码保护 CA 私钥
- 定期轮换 CA 证书（每 5-10 年）

### 2. 客户端证书分发

**风险**：客户端证书泄露后，攻击者可以冒充合法用户。

**缓解措施**：
- 使用强密码保护 PKCS#12 文件
- 通过安全渠道分发证书
- 为每个管理员签发独立的证书（便于审计）

### 3. 证书有效期

**建议**：
- CA 证书：10 年
- 服务器证书：1 年
- 客户端证书：1 年

### 4. 证书吊销

**当前方案**：不支持证书吊销（CRL/OCSP）

**未来改进**：
- 实现简单的证书黑名单
- 或者通过重新签发 CA 证书来吊销所有旧证书

## 参考资料

- [TLS 1.3 RFC 8446](https://tools.ietf.org/html/rfc8446)
- [X.509 Certificate Structure](https://tools.ietf.org/html/rfc5280)
- [mTLS 最佳实践](https://www.cloudflare.com/learning/access-management/what-is-mutual-tls/)

## 更新日志

- 2026-10-22: 初始设计文档
