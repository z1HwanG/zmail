package store

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Blobs 是内容寻址的本地文件存储：
//
//	<root>/raw/<ab>/<sha>.eml.gz
//	<root>/attachments/<ab>/<sha>
type Blobs struct {
	root string
}

// NewBlobs 初始化存储目录。
func NewBlobs(root string) (*Blobs, error) {
	for _, sub := range []string{"raw", "attachments"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o750); err != nil {
			return nil, fmt.Errorf("store: 创建 blob 目录失败: %w", err)
		}
	}
	return &Blobs{root: root}, nil
}

// Root 返回 blob 根目录。
func (b *Blobs) Root() string { return b.root }

func (b *Blobs) path(kind, sha string) string {
	prefix := "00"
	if len(sha) >= 2 {
		prefix = sha[:2]
	}
	name := sha
	if kind == "raw" {
		name += ".eml.gz"
	}
	return filepath.Join(b.root, kind, prefix, name)
}

// Sum 返回内容的 sha256 十六进制摘要。
func Sum(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// PutRaw 以 gzip 压缩保存原始邮件，返回存储键（sha256）。
func (b *Blobs) PutRaw(data []byte) (string, error) {
	sha := Sum(data)
	p := b.path("raw", sha)
	if _, err := os.Stat(p); err == nil {
		return sha, nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	zw := gzip.NewWriter(tmp)
	if _, err := zw.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	if err := zw.Close(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, p); err != nil {
		return "", err
	}
	return sha, nil
}

// OpenRaw 打开（解压）原始邮件。
func (b *Blobs) OpenRaw(sha string) (io.ReadCloser, error) {
	if sha == "" {
		return nil, ErrNotFound
	}
	f, err := os.Open(b.path("raw", sha))
	if err != nil {
		return nil, err
	}
	zr, err := gzip.NewReader(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	return &joinedCloser{Reader: zr, closers: []io.Closer{zr, f}}, nil
}

// PutAttachment 保存附件内容，返回 (sha256, size)。
func (b *Blobs) PutAttachment(data []byte) (string, int64, error) {
	sha := Sum(data)
	p := b.path("attachments", sha)
	if st, err := os.Stat(p); err == nil {
		return sha, st.Size(), nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return "", 0, err
	}
	if err := os.WriteFile(p, data, 0o640); err != nil {
		return "", 0, err
	}
	return sha, int64(len(data)), nil
}

// OpenAttachment 打开附件内容用于下载。
func (b *Blobs) OpenAttachment(sha string) (*os.File, int64, error) {
	f, err := os.Open(b.path("attachments", sha))
	if err != nil {
		return nil, 0, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, st.Size(), nil
}

type joinedCloser struct {
	io.Reader
	closers []io.Closer
}

func (j *joinedCloser) Close() error {
	var first error
	for _, c := range j.closers {
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
