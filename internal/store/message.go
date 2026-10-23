package store

import (
	"bytes"
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/mail"
	"strings"
	"time"
)

const (
	// snippetMaxRunes 是列表摘要的最大字符数。
	snippetMaxRunes = 200
	// 列表默认/最大条数。
	defaultListLimit = 30
	maxListLimit     = 100
)

const messageSelectColumns = `m.id, m.account_id, m.folder_id, COALESCE(m.thread_id, ''),
	m.remote_uid, COALESCE(m.message_id, ''), m.subject, m.from_name, m.from_email,
	m.to_json, m.cc_json, m.bcc_json, m.sent_at, m.received_at, m.snippet, m.body_text,
	m.body_html, m.has_attachments, m.is_read, m.is_starred, m.is_deleted, m.is_draft,
	m.size_bytes, COALESCE(m.raw_key, ''), m.created_at`

const summarySelectColumns = `m.id, m.account_id, a.email, a.display_name, m.folder_id, fo.name,
	m.subject, m.from_name, m.from_email, m.snippet, m.received_at,
	m.is_read, m.is_starred, m.has_attachments`

func scanSummary(sc rowScanner) (*MessageSummary, error) {
	var (
		sm       MessageSummary
		received string
		isRead   int
		isStar   int
		hasAtt   int
	)
	if err := sc.Scan(
		&sm.ID, &sm.AccountID, &sm.AccountEmail, &sm.AccountName, &sm.FolderID, &sm.FolderName,
		&sm.Subject, &sm.FromName, &sm.FromEmail, &sm.Snippet, &received,
		&isRead, &isStar, &hasAtt,
	); err != nil {
		return nil, err
	}
	sm.ReceivedAt = parseTime(received)
	sm.IsRead = isRead != 0
	sm.IsStarred = isStar != 0
	sm.HasAttachments = hasAtt != 0
	return &sm, nil
}

// truncateSnippet 去掉首尾空白并截断到 200 个字符（按 rune 计，兼容中文）。
func truncateSnippet(s string) string {
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if len(runes) <= snippetMaxRunes {
		return s
	}
	return string(runes[:snippetMaxRunes])
}

// normalizeMessageID 归一化 Message-ID：去尖括号与空白、转小写。
func normalizeMessageID(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "<")
	s = strings.TrimSuffix(s, ">")
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return strings.ToLower(s)
}

func firstMessageID(s string) string {
	for _, field := range strings.Fields(s) {
		if id := normalizeMessageID(field); id != "" {
			return id
		}
	}
	return ""
}

var replySubjectPrefixes = []string{"re:", "fw:", "fwd:", "回复:", "答复:", "转发:", "回覆:"}

// normalizeSubject 去掉回复/转发前缀并压缩空白，用于主题线程键。
func normalizeSubject(subject string) string {
	s := strings.TrimSpace(subject)
	for {
		changed := false
		lower := strings.ToLower(s)
		for _, p := range replySubjectPrefixes {
			if strings.HasPrefix(lower, p) {
				s = strings.TrimSpace(s[len(p):])
				changed = true
				break
			}
		}
		if !changed {
			break
		}
	}
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// subjectThreadKey 用归一化主题的 sha1 作为线程键。
func subjectThreadKey(subject string) string {
	normalized := normalizeSubject(subject)
	if normalized == "" {
		normalized = "(no subject)"
	}
	sum := sha1.Sum([]byte(normalized))
	return "subj:" + hex.EncodeToString(sum[:])
}

// rawRootMessageID 从原始邮件头里取会话根：优先 References 首项，其次 In-Reply-To。
func rawRootMessageID(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	parsed, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return ""
	}
	if refs := parsed.Header.Get("References"); refs != "" {
		if id := firstMessageID(refs); id != "" {
			return id
		}
	}
	if irt := parsed.Header.Get("In-Reply-To"); irt != "" {
		if id := firstMessageID(irt); id != "" {
			return id
		}
	}
	return ""
}

// threadKeyFor 计算线程键：优先 In-Reply-To/References 根，否则归一化主题 sha1。
func threadKeyFor(m *Message, raw []byte) string {
	if root := rawRootMessageID(raw); root != "" {
		return "ref:" + root
	}
	return subjectThreadKey(m.Subject)
}

// upsertThread 写入或更新会话，返回 threads.id。
func upsertThread(ctx context.Context, tx *sql.Tx, accountID, threadKey, subject string, at time.Time) (string, error) {
	_, err := tx.ExecContext(ctx, `INSERT INTO threads (id, account_id, thread_key, subject, last_message_at, message_count)
	VALUES (?, ?, ?, ?, ?, 1)
	ON CONFLICT(account_id, thread_key) DO UPDATE SET
		message_count = message_count + 1,
		last_message_at = CASE WHEN excluded.last_message_at > last_message_at
			THEN excluded.last_message_at ELSE last_message_at END,
		subject = excluded.subject`,
		NewID(), accountID, threadKey, subject, formatTime(at))
	if err != nil {
		return "", fmt.Errorf("store: 写入会话失败: %w", err)
	}
	var id string
	if err := tx.QueryRowContext(ctx,
		"SELECT id FROM threads WHERE account_id = ? AND thread_key = ?", accountID, threadKey).Scan(&id); err != nil {
		return "", fmt.Errorf("store: 读取会话主键失败: %w", err)
	}
	return id, nil
}

// InsertMessages 在单事务内批量插入邮件，按 UID 与跨文件夹 Message-ID 幂等去重，返回实际插入数。
func (s *Store) InsertMessages(ctx context.Context, bundles []*MessageBundle) (int, error) {
	if len(bundles) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: 开启事务失败: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().UTC()
	inserted := 0

	for i, bundle := range bundles {
		if bundle == nil || bundle.Message == nil {
			return 0, fmt.Errorf("%w: 第 %d 封邮件为空", ErrInvalid, i)
		}
		m := bundle.Message
		if m.AccountID == "" || m.FolderID == "" {
			return 0, fmt.Errorf("%w: 第 %d 封邮件缺少账号或文件夹", ErrInvalid, i)
		}
		if m.ID == "" {
			m.ID = NewID()
		}

		// 跨文件夹去重：同一账号下相同 Message-ID 只保留一封。
		if m.MessageID != "" {
			var one int
			err := tx.QueryRowContext(ctx,
				"SELECT 1 FROM messages WHERE account_id = ? AND message_id = ? LIMIT 1",
				m.AccountID, m.MessageID).Scan(&one)
			switch {
			case err == nil:
				continue
			case err != sql.ErrNoRows:
				return 0, fmt.Errorf("store: 检查重复邮件失败: %w", err)
			}
		}

		// 同文件夹 UID 去重。
		var one int
		err = tx.QueryRowContext(ctx,
			"SELECT 1 FROM messages WHERE account_id = ? AND folder_id = ? AND remote_uid = ? LIMIT 1",
			m.AccountID, m.FolderID, int64(m.RemoteUID)).Scan(&one)
		switch {
		case err == nil:
			continue
		case err != sql.ErrNoRows:
			return 0, fmt.Errorf("store: 检查重复 UID 失败: %w", err)
		}

		receivedAt := m.ReceivedAt
		if receivedAt.IsZero() {
			receivedAt = now
		}
		createdAt := m.CreatedAt
		if createdAt.IsZero() {
			createdAt = now
		}
		snippet := truncateSnippet(m.Snippet)
		if snippet == "" && m.BodyText != "" {
			snippet = truncateSnippet(m.BodyText)
		}

		rawKey := m.RawKey
		if len(bundle.Raw) > 0 {
			rawKey, err = s.blobs.PutRaw(bundle.Raw)
			if err != nil {
				return 0, fmt.Errorf("store: 保存原始邮件失败: %w", err)
			}
		}
		sizeBytes := m.SizeBytes
		if sizeBytes <= 0 {
			sizeBytes = int64(len(bundle.Raw))
		}
		hasAttachments := len(bundle.Attachments) > 0 || m.HasAttachments

		threadKey := threadKeyFor(m, bundle.Raw)
		threadID, err := upsertThread(ctx, tx, m.AccountID, threadKey, m.Subject, receivedAt)
		if err != nil {
			return 0, err
		}

		res, err := tx.ExecContext(ctx, `INSERT INTO messages
	(id, account_id, folder_id, thread_id, remote_uid, message_id, subject, from_name, from_email,
	 to_json, cc_json, bcc_json, sent_at, received_at, snippet, body_text, body_html,
	 has_attachments, is_read, is_starred, is_deleted, is_draft, size_bytes, raw_key, created_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(account_id, folder_id, remote_uid) DO NOTHING`,
			m.ID, m.AccountID, m.FolderID, threadID, int64(m.RemoteUID), nullIfEmpty(m.MessageID),
			m.Subject, m.FromName, m.FromEmail, marshalAddrs(m.To), marshalAddrs(m.Cc), marshalAddrs(m.Bcc),
			formatTimePtr(m.SentAt), formatTime(receivedAt), snippet, m.BodyText, m.BodyHTML,
			boolInt(hasAttachments), boolInt(m.IsRead), boolInt(m.IsStarred), boolInt(m.IsDeleted), boolInt(m.IsDraft),
			sizeBytes, rawKey, formatTime(createdAt),
		)
		if err != nil {
			return 0, fmt.Errorf("store: 插入邮件失败: %w", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("store: 读取插入结果失败: %w", err)
		}
		if affected == 0 {
			continue
		}

		for _, payload := range bundle.Attachments {
			if payload == nil {
				continue
			}
			sha, attSize, putErr := s.blobs.PutAttachment(payload.Data)
			if putErr != nil {
				return 0, fmt.Errorf("store: 保存附件失败: %w", putErr)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO attachments
	(id, message_id, filename, mime_type, size, content_id, sha256, storage_key)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				NewID(), m.ID, payload.Filename, payload.MimeType, attSize,
				nullIfEmpty(payload.ContentID), sha, sha); err != nil {
				return 0, fmt.Errorf("store: 写入附件失败: %w", err)
			}
		}

		m.ThreadID = threadID
		m.ReceivedAt = receivedAt
		m.CreatedAt = createdAt
		m.Snippet = snippet
		m.SizeBytes = sizeBytes
		m.RawKey = rawKey
		m.HasAttachments = hasAttachments
		inserted++
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: 提交邮件事务失败: %w", err)
	}
	return inserted, nil
}

// nullIfEmpty 把空字符串转成 NULL。
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// GetMessage 读取邮件详情及其附件。
func (s *Store) GetMessage(ctx context.Context, id string) (*Message, []*Attachment, error) {
	if id == "" {
		return nil, nil, fmt.Errorf("%w: 邮件 ID 不能为空", ErrInvalid)
	}
	row := s.db.QueryRowContext(ctx,
		"SELECT "+messageSelectColumns+", a.email, a.display_name, fo.name FROM messages m "+
			"JOIN accounts a ON a.id = m.account_id JOIN folders fo ON fo.id = m.folder_id WHERE m.id = ?", id)
	m, err := scanMessageWithJoin(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil, fmt.Errorf("%w: 邮件 %s", ErrNotFound, id)
		}
		return nil, nil, fmt.Errorf("store: 查询邮件失败: %w", err)
	}

	rows, err := s.db.QueryContext(ctx, `SELECT id, message_id, filename, mime_type, size,
		COALESCE(content_id, ''), sha256, storage_key FROM attachments WHERE message_id = ? ORDER BY rowid ASC`, id)
	if err != nil {
		return nil, nil, fmt.Errorf("store: 查询附件失败: %w", err)
	}
	defer rows.Close()

	attachments := make([]*Attachment, 0, 2)
	for rows.Next() {
		var a Attachment
		if err := rows.Scan(&a.ID, &a.MessageID, &a.Filename, &a.MimeType, &a.Size,
			&a.ContentID, &a.SHA256, &a.StorageKey); err != nil {
			return nil, nil, fmt.Errorf("store: 扫描附件失败: %w", err)
		}
		attachments = append(attachments, &a)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("store: 遍历附件失败: %w", err)
	}
	return m, attachments, nil
}

// scanMessageWithJoin 扫描带账号/文件夹 JOIN 字段的邮件行。
func scanMessageWithJoin(sc rowScanner) (*Message, error) {
	var (
		m        Message
		sentAt   sql.NullString
		toJSON   string
		ccJSON   string
		bccJSON  string
		hasAtt   int
		isRead   int
		isStar   int
		isDel    int
		isDraft  int
		received string
		created  string
	)
	if err := sc.Scan(
		&m.ID, &m.AccountID, &m.FolderID, &m.ThreadID, &m.RemoteUID, &m.MessageID,
		&m.Subject, &m.FromName, &m.FromEmail, &toJSON, &ccJSON, &bccJSON,
		&sentAt, &received, &m.Snippet, &m.BodyText, &m.BodyHTML,
		&hasAtt, &isRead, &isStar, &isDel, &isDraft,
		&m.SizeBytes, &m.RawKey, &created,
		&m.AccountEmail, &m.AccountName, &m.FolderName,
	); err != nil {
		return nil, err
	}
	m.To = unmarshalAddrs(toJSON)
	m.Cc = unmarshalAddrs(ccJSON)
	m.Bcc = unmarshalAddrs(bccJSON)
	m.SentAt = parseTimePtr(nullStringPtr(sentAt))
	m.ReceivedAt = parseTime(received)
	m.CreatedAt = parseTime(created)
	m.HasAttachments = hasAtt != 0
	m.IsRead = isRead != 0
	m.IsStarred = isStar != 0
	m.IsDeleted = isDel != 0
	m.IsDraft = isDraft != 0
	return &m, nil
}

// parseCursor 解析 "receivedAt|id" 游标。
func parseCursor(cursor string) (time.Time, string, bool) {
	idx := strings.Index(cursor, "|")
	if idx <= 0 || idx == len(cursor)-1 {
		return time.Time{}, "", false
	}
	at := parseTime(cursor[:idx])
	if at.IsZero() {
		return time.Time{}, "", false
	}
	id := cursor[idx+1:]
	if id == "" {
		return time.Time{}, "", false
	}
	return at, id, true
}

// formatCursor 生成 "receivedAt|id" 游标。
func formatCursor(receivedAt time.Time, id string) string {
	return formatTime(receivedAt) + "|" + id
}

// ListMessages 按游标分页列出邮件摘要，返回列表与 nextCursor。
func (s *Store) ListMessages(ctx context.Context, f ListFilter) ([]*MessageSummary, string, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}

	where := []string{"m.is_deleted = 0"}
	args := make([]any, 0, 8)
	if f.AccountID != "" {
		where = append(where, "m.account_id = ?")
		args = append(args, f.AccountID)
	}
	if f.FolderID != "" {
		where = append(where, "m.folder_id = ?")
		args = append(args, f.FolderID)
	}
	if f.Scope == "inbox" {
		where = append(where, "fo.role = ?")
		args = append(args, RoleInbox)
	}
	if f.UnreadOnly {
		where = append(where, "m.is_read = 0")
	}
	if f.StarredOnly {
		where = append(where, "m.is_starred = 1")
	}
	if !f.Before.IsZero() {
		where = append(where, "m.received_at < ?")
		args = append(args, formatTime(f.Before))
	}
	if f.Cursor != "" {
		at, id, ok := parseCursor(f.Cursor)
		if !ok {
			return nil, "", fmt.Errorf("%w: 分页游标格式错误", ErrInvalid)
		}
		where = append(where, "(m.received_at < ? OR (m.received_at = ? AND m.id < ?))")
		args = append(args, formatTime(at), formatTime(at), id)
	}

	query := "SELECT " + summarySelectColumns + " FROM messages m " +
		"JOIN accounts a ON a.id = m.account_id JOIN folders fo ON fo.id = m.folder_id " +
		"WHERE " + strings.Join(where, " AND ") +
		" ORDER BY m.received_at DESC, m.id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", fmt.Errorf("store: 查询邮件列表失败: %w", err)
	}
	defer rows.Close()

	out := make([]*MessageSummary, 0, limit)
	for rows.Next() {
		item, scanErr := scanSummary(rows)
		if scanErr != nil {
			return nil, "", fmt.Errorf("store: 扫描邮件摘要失败: %w", scanErr)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("store: 遍历邮件列表失败: %w", err)
	}

	next := ""
	if len(out) == limit && len(out) > 0 {
		last := out[len(out)-1]
		next = formatCursor(last.ReceivedAt, last.ID)
	}
	return out, next, nil
}

// SetMessageFlags 按需更新已读/星标，返回更新后的邮件。
func (s *Store) SetMessageFlags(ctx context.Context, id string, isRead, isStarred *bool) (*Message, error) {
	if id == "" {
		return nil, fmt.Errorf("%w: 邮件 ID 不能为空", ErrInvalid)
	}
	sets := make([]string, 0, 2)
	args := make([]any, 0, 3)
	if isRead != nil {
		sets = append(sets, "is_read = ?")
		args = append(args, boolInt(*isRead))
	}
	if isStarred != nil {
		sets = append(sets, "is_starred = ?")
		args = append(args, boolInt(*isStarred))
	}
	if len(sets) > 0 {
		args = append(args, id)
		res, err := s.db.ExecContext(ctx, "UPDATE messages SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
		if err != nil {
			return nil, fmt.Errorf("store: 更新邮件标记失败: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("store: 更新邮件标记失败: %w", err)
		}
		if n == 0 {
			return nil, fmt.Errorf("%w: 邮件 %s", ErrNotFound, id)
		}
	}
	msg, _, err := s.GetMessage(ctx, id)
	if err != nil {
		return nil, err
	}
	return msg, nil
}

// SoftDeleteMessage 软删除邮件（保留记录，仅置 is_deleted=1）。
func (s *Store) SoftDeleteMessage(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("%w: 邮件 ID 不能为空", ErrInvalid)
	}
	res, err := s.db.ExecContext(ctx, "UPDATE messages SET is_deleted = 1 WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("store: 软删除邮件失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: 软删除邮件失败: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: 邮件 %s", ErrNotFound, id)
	}
	return nil
}

// HardDeleteMessage 物理删除邮件并维护会话计数。
func (s *Store) HardDeleteMessage(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("%w: 邮件 ID 不能为空", ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: 开启事务失败: %w", err)
	}
	defer tx.Rollback()

	var threadID sql.NullString
	if err := tx.QueryRowContext(ctx, "SELECT thread_id FROM messages WHERE id = ?", id).Scan(&threadID); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("%w: 邮件 %s", ErrNotFound, id)
		}
		return fmt.Errorf("store: 查询邮件失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM messages WHERE id = ?", id); err != nil {
		return fmt.Errorf("store: 删除邮件失败: %w", err)
	}
	if threadID.Valid && threadID.String != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE threads SET
			message_count = (SELECT COUNT(*) FROM messages WHERE thread_id = ?),
			last_message_at = COALESCE((SELECT MAX(received_at) FROM messages WHERE thread_id = ?), last_message_at)
			WHERE id = ?`, threadID.String, threadID.String, threadID.String); err != nil {
			return fmt.Errorf("store: 更新会话计数失败: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM threads WHERE id = ? AND message_count <= 0", threadID.String); err != nil {
			return fmt.Errorf("store: 清理空会话失败: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: 提交删除事务失败: %w", err)
	}
	return nil
}

// MessageExists 判断同一账号下是否已存在指定 Message-ID。
func (s *Store) MessageExists(ctx context.Context, accountID, messageID string) (bool, error) {
	if accountID == "" {
		return false, fmt.Errorf("%w: 账号 ID 不能为空", ErrInvalid)
	}
	if messageID == "" {
		return false, nil
	}
	var exists int
	if err := s.db.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM messages WHERE account_id = ? AND message_id = ?)",
		accountID, messageID).Scan(&exists); err != nil {
		return false, fmt.Errorf("store: 查询邮件是否存在失败: %w", err)
	}
	return exists != 0, nil
}

// UnreadCounts 按文件夹汇总未读数。
func (s *Store) UnreadCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT folder_id, COUNT(*) FROM messages WHERE is_read = 0 AND is_deleted = 0 GROUP BY folder_id")
	if err != nil {
		return nil, fmt.Errorf("store: 统计未读数失败: %w", err)
	}
	defer rows.Close()

	out := make(map[string]int, 8)
	for rows.Next() {
		var folderID string
		var n int
		if err := rows.Scan(&folderID, &n); err != nil {
			return nil, fmt.Errorf("store: 扫描未读数失败: %w", err)
		}
		out[folderID] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历未读数失败: %w", err)
	}
	return out, nil
}

// UnreadTotal 返回全部账号的未读邮件总数。
func (s *Store) UnreadTotal(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM messages WHERE is_read = 0 AND is_deleted = 0").Scan(&n); err != nil {
		return 0, fmt.Errorf("store: 统计未读总数失败: %w", err)
	}
	return n, nil
}

// RecentMessages 返回最近的邮件摘要。
func (s *Store) RecentMessages(ctx context.Context, accountID string, limit int) ([]*MessageSummary, error) {
	if limit <= 0 {
		limit = 10
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}
	query := "SELECT " + summarySelectColumns + " FROM messages m " +
		"JOIN accounts a ON a.id = m.account_id JOIN folders fo ON fo.id = m.folder_id WHERE m.is_deleted = 0"
	args := make([]any, 0, 2)
	if accountID != "" {
		query += " AND m.account_id = ?"
		args = append(args, accountID)
	}
	query += " ORDER BY m.received_at DESC, m.id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 查询最近邮件失败: %w", err)
	}
	defer rows.Close()

	out := make([]*MessageSummary, 0, limit)
	for rows.Next() {
		item, scanErr := scanSummary(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("store: 扫描邮件摘要失败: %w", scanErr)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历最近邮件失败: %w", err)
	}
	return out, nil
}
