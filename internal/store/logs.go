package store

import (
	"context"
	"fmt"
	"time"
)

const syncLogColumns = `id, account_id, level, message, created_at`

// AddSyncLog 追加一条同步日志。
func (s *Store) AddSyncLog(ctx context.Context, accountID, level, message string) error {
	if level == "" {
		level = "info"
	}
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO sync_logs (id, account_id, level, message, created_at) VALUES (?, ?, ?, ?, ?)",
		NewID(), accountID, level, message, formatTime(time.Now().UTC()))
	if err != nil {
		return fmt.Errorf("store: 写入同步日志失败: %w", err)
	}
	return nil
}

// ListSyncLogs 按时间倒序列出同步日志；accountID 为空时返回全部。
func (s *Store) ListSyncLogs(ctx context.Context, accountID string, limit int) ([]*SyncLog, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	query := "SELECT " + syncLogColumns + " FROM sync_logs"
	args := make([]any, 0, 2)
	if accountID != "" {
		query += " WHERE account_id = ?"
		args = append(args, accountID)
	}
	query += " ORDER BY created_at DESC, rowid DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 查询同步日志失败: %w", err)
	}
	defer rows.Close()

	out := make([]*SyncLog, 0, limit)
	for rows.Next() {
		var (
			l       SyncLog
			created string
		)
		if err := rows.Scan(&l.ID, &l.AccountID, &l.Level, &l.Message, &created); err != nil {
			return nil, fmt.Errorf("store: 扫描同步日志失败: %w", err)
		}
		l.CreatedAt = parseTime(created)
		out = append(out, &l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历同步日志失败: %w", err)
	}
	return out, nil
}
