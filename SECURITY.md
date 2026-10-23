# Security Policy

## 支持版本

仅对最新发布版本提供安全修复，当前支持 `0.1.x`。

## 报告漏洞

请不要在公开 Issue 中披露安全问题、邮箱凭据、会话密钥或可利用细节。

优先使用 GitHub 的 **Private vulnerability reporting**：

<https://github.com/z1HwanG/zmail/security/advisories/new>

如果该入口不可用，请通过仓库所有者公开资料中的联系方式私下联系，并在标题中注明 `SECURITY`。

报告时建议附上：

- 受影响版本
- 复现步骤或最小 PoC
- 影响范围
- 建议修复方式（如有）

## 部署安全基线

- `PUBLIC_URL` 使用实际 HTTPS 地址
- `SESSION_KEY`、`CREDENTIALS_KEY` 使用独立随机密钥并妥善备份
- 不要将 `.env`、`data/`、数据库、证书或密钥提交到仓库
- 公网只暴露反向代理的 80/443，`LISTEN_ADDR` 保持回环地址
- `TRUSTED_PROXY_CIDRS` 只填写可信反代网段，禁止 `0.0.0.0/0`
- 定期备份 `DB_PATH` 与 `RAW_DIR`，并限制文件权限

## 已知边界

- 本项目为单用户设计，不提供多租户隔离
- 邮箱服务商的应用密码/授权码权限由服务商控制
- 反向代理、TLS 证书和主机安全由部署者负责
