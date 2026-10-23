// Package web 只负责携带前端静态资源，不依赖项目内其他包。
package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"sort"
	"sync"
)

// assets 是编译进二进制的静态资源目录；目录结构即 URL /static/ 下的结构。
//
//go:embed static
var assets embed.FS

// Static 返回以 static 目录为根的只读文件系统，供 http.FileServerFS 或
// http.ServeFileFS 挂载到 /static/ 前缀。
func Static() fs.FS {
	sub, err := fs.Sub(assets, "static")
	if err != nil {
		// embed.FS 的 static 目录在编译期已确定，此分支不可达；
		// 不使用 panic 作为控制流，返回空文件系统让上层返回 404。
		return fs.FS(emptyFS{})
	}
	return sub
}

var (
	versionOnce sync.Once
	version     string
)

// Version 返回静态资源内容指纹（12 位十六进制）。
// 模板把 ?v=<Version> 拼到静态资源 URL 上，升级后浏览器和 Service Worker
// 会自动请求新资源，避免继续使用缓存里的旧 app.js/app.css。
func Version() string {
	versionOnce.Do(func() {
		root := Static()
		paths := make([]string, 0, 16)
		_ = fs.WalkDir(root, ".", func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			paths = append(paths, path)
			return nil
		})
		sort.Strings(paths)
		h := sha256.New()
		for _, p := range paths {
			data, err := fs.ReadFile(root, p)
			if err != nil {
				continue
			}
			h.Write([]byte(p))
			h.Write([]byte{0})
			h.Write(data)
			h.Write([]byte{0})
		}
		version = hex.EncodeToString(h.Sum(nil))[:12]
	})
	return version
}

type emptyFS struct{}

func (emptyFS) Open(string) (fs.File, error) { return nil, fs.ErrNotExist }
