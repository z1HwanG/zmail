package store

import (
	"context"
	"database/sql"
	"fmt"
)

// GetSetting 读取单项配置；不存在时返回 ("", false, nil)。
func (s *Store) GetSetting(ctx context.Context, key string) (string, bool, error) {
	if key == "" {
		return "", false, fmt.Errorf("%w: 配置键不能为空", ErrInvalid)
	}
	var value string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", key).Scan(&value)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", false, nil
		}
		return "", false, fmt.Errorf("store: 读取配置失败: %w", err)
	}
	return value, true, nil
}

// SetSetting 写入或覆盖单项配置。
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	if key == "" {
		return fmt.Errorf("%w: 配置键不能为空", ErrInvalid)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?)
	ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("store: 写入配置失败: %w", err)
	}
	return nil
}

// ListSettings 返回全部配置。
func (s *Store) ListSettings(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT key, value FROM settings ORDER BY key ASC")
	if err != nil {
		return nil, fmt.Errorf("store: 查询配置失败: %w", err)
	}
	defer rows.Close()

	out := make(map[string]string, 8)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("store: 扫描配置失败: %w", err)
		}
		out[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历配置失败: %w", err)
	}
	return out, nil
}
