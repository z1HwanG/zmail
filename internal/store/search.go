package store

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"
)

// escapeLike 转义 LIKE 通配符，使用 '!' 作为 ESCAPE 字符。
func escapeLike(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range s {
		switch r {
		case '!', '%', '_':
			b.WriteRune('!')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Search 全文检索邮件；优先 FTS5 trigram，查询过短或语法错误时退回 LIKE。
func (s *Store) Search(ctx context.Context, query, accountID string, limit int) ([]*MessageSummary, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}

	runeLen := utf8.RuneCountInString(q)
	if runeLen >= 2 {
		out, err := s.searchFTS(ctx, q, accountID, limit)
		if err == nil {
			// trigram 对少于 3 个字符的查询无法命中，此时补一次 LIKE。
			if len(out) > 0 || runeLen >= 3 {
				return out, nil
			}
		}
	}
	return s.searchLike(ctx, q, accountID, limit)
}

// searchFTS 用 FTS5 trigram MATCH 查询，查询串整体加双引号并按 FTS 规则转义。
func (s *Store) searchFTS(ctx context.Context, query, accountID string, limit int) ([]*MessageSummary, error) {
	quote := string(rune(34))
	matchQuery := quote + strings.ReplaceAll(query, quote, quote+quote) + quote

	sqlText := "SELECT " + summarySelectColumns + " FROM messages_fts " +
		"JOIN messages m ON m.rowid = messages_fts.rowid " +
		"JOIN accounts a ON a.id = m.account_id JOIN folders fo ON fo.id = m.folder_id " +
		"WHERE messages_fts MATCH ? AND m.is_deleted = 0"
	args := []any{matchQuery}
	if accountID != "" {
		sqlText += " AND m.account_id = ?"
		args = append(args, accountID)
	}
	sqlText += " ORDER BY messages_fts.rank, m.received_at DESC, m.id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 全文检索失败: %w", err)
	}
	defer rows.Close()
	return scanSummaryRows(rows)
}

// searchLike 用 LIKE 做子串回退检索。
func (s *Store) searchLike(ctx context.Context, query, accountID string, limit int) ([]*MessageSummary, error) {
	pattern := "%" + escapeLike(query) + "%"
	sqlText := "SELECT " + summarySelectColumns + " FROM messages m " +
		"JOIN accounts a ON a.id = m.account_id JOIN folders fo ON fo.id = m.folder_id " +
		"WHERE m.is_deleted = 0 AND (m.subject LIKE ? ESCAPE '!' OR m.body_text LIKE ? ESCAPE '!' " +
		"OR m.from_name LIKE ? ESCAPE '!' OR m.from_email LIKE ? ESCAPE '!')"
	args := []any{pattern, pattern, pattern, pattern}
	if accountID != "" {
		sqlText += " AND m.account_id = ?"
		args = append(args, accountID)
	}
	sqlText += " ORDER BY m.received_at DESC, m.id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 模糊检索失败: %w", err)
	}
	defer rows.Close()
	return scanSummaryRows(rows)
}

// scanSummaryRows 把结果集扫描为摘要列表。
func scanSummaryRows(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}) ([]*MessageSummary, error) {
	out := make([]*MessageSummary, 0, defaultListLimit)
	for rows.Next() {
		item, err := scanSummary(rows)
		if err != nil {
			return nil, fmt.Errorf("store: 扫描检索结果失败: %w", err)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历检索结果失败: %w", err)
	}
	return out, nil
}
