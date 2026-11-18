# 2026-11-18 敏感信息审计与历史清理

## 当前树清理

发现并移除以下 tracked 文件：

- `client-all.p12`、`client.p12`：客户端证书 + 私钥容器
- `--db`：本地误生成的数据库快照垃圾

本地文件已移动到 `archive/sensitive-20261118/` 与 `archive/tmp-20261118/`（`archive/` 已忽略，不删除）。
`config.yaml`（本地含代理密码/私钥）也移到 `archive/sensitive-20261118/`，仓库中始终未跟踪。

`.gitignore` 新增：

```
*.p12
*.pfx
*.jks
*.key
/data/
/start-phaethon.vbs
/--db
```

`config.yaml`、`archive/`、`scripts/.env`、`scripts/signing.key`、日志/二进制此前已在忽略列表。

## 历史清理

`client-all.p12` / `client.p12` / `--db` 是在 `aec7a5c`（旧 hash）加入历史的。
使用 `git filter-repo --invert-paths` 从所有 refs 中移除，并强制推送：

- 新 `master`：`610b35d`
- 新 `v0.8.2` tag：`635dd1e`（peeled `9b8ea6a`）

清理后 `git log --all -- client-all.p12 client.p12 --db` 为空。
旧提交对象在本地被 repack/prune；GitHub 端不可达对象可能在服务端缓存一段时间后才 GC，
如需彻底清除需联系 GitHub 支持。

**建议**：`client-all.p12` 的客户端证书/私钥曾进入历史，若仓库曾对非完全可信人员可见，
用同一 CA 重新签发客户端证书并替换各节点/本地客户端（CA 本身按此前决定暂不更换）。

## 其余扫描结论

- 当前树无 `BEGIN PRIVATE KEY` 实钥；`admin/templates/proxies.html` 的 marker 是输入框 placeholder。
- 文档/测试中的 `password=xxx`、`changeme`、`your-secret-token`、`testpass` 均为占位/测试值，不是真实凭据。
- 公开材料仍在仓库：`ca.crt`、`client*.crt`、`client-all.csr`、`admin.pub`、`tools/client-all.crt`
  （均为证书/CSR/公钥，不含私钥）。
- **91 个 docs 文件仍包含 IP 地址**（内网/公网运维记录，如 `docs/issues/*`、`docs/plans/*`）。
  这些不是凭据，但按 README 的安全策略属于敏感信息；本次未自动批量替换，待决定是否统一改为
  占位符（会影响问题记录的现场细节）。
- `docs/issues/data-migration-analysis.md` 含有真实订阅节点域名（非密码），同属待处理项。

## 历史 hash 说明

本次重写后，`docs/` 中引用的旧 commit hash（如 `b10729f`、`d67b41a`、`10cb7fc`）仅作历史记录，
不再对应远端当前 hash；功能/发布结论不受影响，`v0.8.2` 版本名与已部署二进制不受影响。
