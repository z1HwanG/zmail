# zMail 常用命令
BINARY := zmail
PKG    := ./cmd/zmail
GOFLAGS := -trimpath
LDFLAGS := -s -w -X main.version=$(shell git describe --tags --always 2>/dev/null || echo dev)

.PHONY: all build build-linux vet fmt run tidy clean sqlite genkey check

all: build

build:
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BINARY) $(PKG)

# 交叉编译 Linux 单文件二进制（服务器部署用）
build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o dist/zmail-linux-amd64 $(PKG)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o dist/zmail-linux-arm64 $(PKG)
	@ls -lh dist/

vet:
	go vet ./...

fmt:
	gofmt -l -w .

run:
	go run $(PKG)

tidy:
	go mod tidy

clean:
	rm -f $(BINARY)
	rm -rf dist

genkey:
	go run $(PKG) -genkey

sqlite:
	sqlite3 data/zmail.db ".tables"

check:
	CGO_ENABLED=0 go build $(GOFLAGS) -o /dev/null $(PKG)
	go vet ./...
