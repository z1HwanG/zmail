# Contributing

感谢关注 zMail。提交代码前请先确认改动范围，避免引入凭据、邮箱数据或与本项目目标无关的依赖。

## 环境要求

- Go 1.26+
- 无需 CGO、Node.js 或外部数据库
- Windows 可使用 `make.cmd`，Linux/macOS 可使用 `make`

## 本地检查

```bash
gofmt -l -w .
go vet ./...
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o zmail ./cmd/zmail
```

Windows：

```bat
make.cmd check
```

仓库当前不包含自动化测试，涉及同步、发信、鉴权、HTML 消毒与反代的改动，请按 `docs/BUILD.md` 的手工验收清单补充验证说明。

## 提交规范

- 提交前确认 `git status` 中没有 `.env`、`data/`、数据库、日志、证书或构建产物
- 示例配置统一使用 `example.com`、`change-me` 等占位值
- 不提交真实邮箱地址、授权码、Token、Cookie 或服务器 IP
- 保持 Go 代码通过 `gofmt` 和 `go vet`
- 涉及用户可见行为时同步更新 README、CHANGELOG 或相关文档

## Pull Request

请在 PR 中说明：

1. 改了什么
2. 为什么改
3. 如何验证
4. 是否有配置、接口或部署方式变更

安全问题请遵循 [SECURITY.md](SECURITY.md)，不要通过公开 PR 披露。
