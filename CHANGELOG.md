# Changelog

本项目使用版本号 `主版本.次版本.修订号` 标记发布，日期使用 UTC。

## [Unreleased]

### Added

- 新增 MIT License

### Changed

- 部署文档迁移为 `docs/DEPLOY.md`，补充 Docker Compose 完整部署流程

## [0.1.1] - 2026-10-24

首个公开版本。

### Added

- 多账号 IMAP 聚合收件箱，支持 Gmail、Outlook、QQ、163 与自建邮箱
- IMAP IDLE 秒级收信，不支持 IDLE 的服务商自动降级为短轮询
- UIDVALIDITY 检测、断线指数退避、按 UID 增量补齐
- 游标分页与后台历史回填
- 验证码识别、浏览器通知与一键复制
- SMTP 发信、回复、转发、多附件，失败自动入队重试
- 已读/星标/删除回写 IMAP
- AES-256-GCM 凭据加密、HMAC 会话、CSRF Origin 校验
- 服务端 HTML 白名单消毒、CSP、SSRF 防护与图片代理
- 响应式三栏界面、深色模式、快捷键、命令面板与 PWA
- systemd、Docker Compose 与 1Panel 部署文档

### Changed

- 整理项目目录，移除前端原型和设计迭代截图
- 更新 README、构建文档、接口契约和部署说明
- 脱敏示例配置与本机开发脚本，避免提交真实凭据

### Removed

- 历史自动化测试代码与冒烟脚本
- 构建产物、重复环境变量示例和临时设计文件

### Notes

- 当前版本不附带自动化测试，发布前请按 `docs/BUILD.md` 的手工验收清单验证
- 运行时数据 `data/`、`.env`、证书和密钥均不会提交到仓库
