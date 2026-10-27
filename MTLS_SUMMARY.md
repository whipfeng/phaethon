# mTLS 双向认证实现总结

## 已完成

✅ **实现 mTLS 双向 TLS 认证**
- 用客户端证书替代用户名/密码登录
- 完全隐藏登录页面
- 基于证书的安全认证

✅ **代码修改**
1. `config/config.go` - 添加 `ClientCA` 字段
2. `admin/admin.go` - TLS 配置支持 mTLS + 认证中间件检查客户端证书
3. `tools/phaethon-cert.go` - 证书生成工具
4. `tools/MTLS_GUIDE.md` - 完整使用文档

✅ **编译验证**
- Windows 版本编译成功
- 证书工具编译成功
- 已生成测试证书（ca.crt, client.crt 等）

## 使用方法

### 1. 生成证书

```bash
# 生成 CA 证书（只需一次）
./dist/phaethon-cert ca

# 生成客户端证书
./dist/phaethon-cert client ca.key ca.crt
```

### 2. 配置服务器

```bash
# 在 VM 上设置 CA 证书
./phaethon-db set-client-ca ca.crt

# 重启服务
./phaethon restart
```

### 3. 安装客户端证书到浏览器

```bash
# 转换为 PKCS#12 格式
openssl pkcs12 -export -out client.p12 -inkey client.key -in client.crt
# 输入密码（例如：changeme）

# 导入到浏览器
# Chrome/Edge: 设置 → 隐私和安全 → 安全 → 管理证书 → 导入
# Firefox: 设置 → 隐私与安全 → 安全 → 证书 → 导入
```

### 4. 访问管理员面板

```bash
# 浏览器访问
https://vm.phn

# 浏览器会提示选择客户端证书
# 选择刚导入的证书
# 自动登录，无需用户名密码！
```

## 认证优先级

1. **mTLS 客户端证书**（最高优先级）
2. Session 认证（已登录的浏览器会话）
3. 签名认证（X-Signature + X-Timestamp）
4. Token 认证（Bearer token）

## 文件说明

**已生成的证书（测试用）：**
- `ca.key` - CA 私钥（保密！）
- `ca.crt` - CA 证书（配置到服务器）
- `client.key` - 客户端私钥（安装到浏览器）
- `client.crt` - 客户端证书（安装到浏览器）

**工具：**
- `dist/phaethon-cert` - 证书生成工具
- `dist/phaethon-db` - 数据库管理工具

**文档：**
- `tools/MTLS_GUIDE.md` - 完整使用指南

## 下一步

由于 VM SSH 不稳定，需要等待 VM 恢复后：

1. **部署新版本到 VM**
   ```bash
   make windows
   scp -P 39022 dist/windows-amd64/phaethon.exe Docker@10.21.20.65:/c/temp/
   # stop + replace + start
   ```

2. **配置 mTLS**
   ```bash
   # 上传 CA 证书到 VM
   scp -P 39022 ca.crt Docker@10.21.20.65:/c/temp/
   
   # 在 VM 上设置
   ./phaethon-db set-client-ca ca.crt
   ./phaethon restart
   ```

3. **测试 mTLS**
   ```bash
   # 转换客户端证书
   openssl pkcs12 -export -out client.p12 -inkey client.key -in client.crt
   
   # 导入到浏览器，访问 https://vm.phn
   ```

## 技术细节

**证书规格：**
- CA：ECDSA P-256，10 年有效期
- 客户端：ECDSA P-256，5 年有效期
- 签名算法：SHA-256

**TLS 配置：**
```go
s.tlsConfig = &tls.Config{
    Certificates: []tls.Certificate{cert},
    ClientCAs:    caCertPool,           // CA 证书池
    ClientAuth:   tls.RequireAndVerifyClientCert, // 要求客户端证书
}
```

**认证流程：**
1. 浏览器发送 ClientHello
2. 服务器返回 CertificateRequest（请求客户端证书）
3. 浏览器发送客户端证书
4. 服务器验证证书（使用 CA）
5. 验证通过 → authMiddleware 检查 r.TLS.PeerCertificates
6. 自动登录，无需账密

## 优势

✅ **安全性高** - 基于证书，无法暴力破解
✅ **用户体验好** - 一次导入，自动登录
✅ **适合自动化** - CI/CD 可以使用客户端证书
✅ **可追溯** - 每个客户端可以有独立证书
✅ **隐藏登录页** - 无证书访问直接拒绝

## 注意事项

1. **CA 私钥必须保密** - 泄露后需要重新生成所有证书
2. **证书有效期** - CA 10 年，客户端 5 年
3. **浏览器兼容性** - 所有现代浏览器都支持
4. **备份** - 建议备份 CA 证书和私钥
5. **证书撤销** - 需要重新生成 CA 并重新分发
