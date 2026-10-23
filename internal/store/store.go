// Package store 是 zMail 的持久层：SQLite（WAL）+ 原始邮件/附件 blob 目录。
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// ErrNotFound 表示记录不存在。
var ErrNotFound = errors.New("store: 记录不存在")

// ErrInvalid 表示入参不合法。
var ErrInvalid = errors.New("store: 参数不合法")

// Store 封装数据库连接与 blob 存储。
type Store struct {
	db    *sql.DB
	blobs *Blobs
}

// Open 打开（或创建）数据库并执行建表语句。
func Open(dbPath, rawDir string) (*Store, error) {
	if dbPath == "" {
		return nil, fmt.Errorf("store: DB_PATH 不能为空")
	}
	if rawDir == "" {
		rawDir = filepath.Join(filepath.Dir(dbPath), "raw")
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o750); err != nil {
		return nil, fmt.Errorf("store: 创建数据库目录失败: %w", err)
	}

	dsn := buildDSN(dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: 打开数据库失败: %w", err)
	}
	// 单写者：所有语句串行执行，避免 database is locked。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: 连接数据库失败: %w", err)
	}

	blobs, err := NewBlobs(rawDir)
	if err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db, blobs: blobs}
	if err := s.Migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func buildDSN(dbPath string) string {
	params := "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_pragma=temp_store(MEMORY)"
	if dbPath == ":memory:" || strings.HasPrefix(dbPath, "file::memory:") {
		return "file::memory:?cache=shared&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	}
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		abs = dbPath
	}
	return "file:" + filepath.ToSlash(abs) + params
}

// Migrate 执行幂等的建表/索引/触发器语句，并补齐后续版本新增的列。
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("store: 初始化 schema 失败: %w", err)
	}
	// v1.1：folders.backfill_uid 记录历史回填游标（老库需要 ALTER TABLE 补齐）。
	has, err := s.hasColumn(ctx, "folders", "backfill_uid")
	if err != nil {
		return err
	}
	if !has {
		if _, err := s.db.ExecContext(ctx,
			"ALTER TABLE folders ADD COLUMN backfill_uid INTEGER NOT NULL DEFAULT 0"); err != nil {
			return fmt.Errorf("store: 添加 folders.backfill_uid 列失败: %w", err)
		}
	}
	return nil
}

// hasColumn 判断表是否已有指定列。
func (s *Store) hasColumn(ctx context.Context, table, column string) (bool, error) {
	rows, err := s.db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return false, fmt.Errorf("store: 读取表结构失败: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid       int
			name      string
			ctype     string
			notNull   int
			dfltValue sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dfltValue, &pk); err != nil {
			return false, fmt.Errorf("store: 解析表结构失败: %w", err)
		}
		if strings.EqualFold(name, column) {
			return true, nil
		}
	}
	return false, rows.Err()
}

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

// DB 返回底层连接（供少量特殊查询使用）。
func (s *Store) DB() *sql.DB { return s.db }

// Blobs 返回原始邮件/附件存储。
func (s *Store) Blobs() *Blobs { return s.blobs }

// NewID 生成 16 字节随机十六进制 ID。
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// Ping 检查数据库可用性。
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Stats 汇总账号/文件夹/邮件计数。
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	row := s.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM accounts),
		(SELECT COUNT(*) FROM folders),
		(SELECT COUNT(*) FROM messages WHERE is_deleted = 0),
		(SELECT COUNT(*) FROM messages WHERE is_read = 0 AND is_deleted = 0),
		(SELECT COUNT(*) FROM messages WHERE is_starred = 1 AND is_deleted = 0)`)
	if err := row.Scan(&st.Accounts, &st.Folders, &st.Messages, &st.Unread, &st.Starred); err != nil {
		return st, fmt.Errorf("store: 统计失败: %w", err)
	}
	return st, nil
}

// CleanupRuntimeData 清理过期的同步日志与已完成任务。
func (s *Store) CleanupRuntimeData(ctx context.Context, logRetention, jobRetention time.Duration) (int64, int64, error) {
	logCutoff := time.Now().UTC().Add(-logRetention)
	jobCutoff := time.Now().UTC().Add(-jobRetention)
	res, err := s.db.ExecContext(ctx, `DELETE FROM sync_logs WHERE created_at < ?`, formatTime(logCutoff))
	if err != nil {
		return 0, 0, fmt.Errorf("store: 清理 sync_logs 失败: %w", err)
	}
	nLogs, _ := res.RowsAffected()
	res, err = s.db.ExecContext(ctx, `DELETE FROM jobs WHERE status IN ('done','failed') AND created_at < ?`, formatTime(jobCutoff))
	if err != nil {
		return nLogs, 0, fmt.Errorf("store: 清理 jobs 失败: %w", err)
	}
	nJobs, _ := res.RowsAffected()
	return nLogs, nJobs, nil
}
