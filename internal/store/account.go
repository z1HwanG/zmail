package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// rowScanner 抽象 *sql.Row / *sql.Rows 的 Scan 方法，便于复用扫描逻辑。
type rowScanner interface {
	Scan(dest ...any) error
}

const accountColumns = `id, provider, email, display_name, username, password_enc, imap_host, imap_port, imap_secure, smtp_host, smtp_port, smtp_secure, status, last_error, last_synced_at, created_at`

// nullStringPtr 把 sql.NullString 转成 *string，无效值返回 nil。
func nullStringPtr(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	return &n.String
}

// isUniqueViolation 判断错误是否为 SQLite 唯一约束冲突。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") || strings.Contains(msg, "constraint failed: UNIQUE")
}

func scanAccount(sc rowScanner) (*Account, error) {
	var (
		a        Account
		lastSync sql.NullString
		created  string
		imapSec  int
		smtpSec  int
	)
	if err := sc.Scan(
		&a.ID, &a.Provider, &a.Email, &a.DisplayName, &a.Username, &a.PasswordEnc,
		&a.IMAPHost, &a.IMAPPort, &imapSec, &a.SMTPHost, &a.SMTPPort, &smtpSec,
		&a.Status, &a.LastError, &lastSync, &created,
	); err != nil {
		return nil, err
	}
	a.IMAPSecure = imapSec != 0
	a.SMTPSecure = smtpSec != 0
	a.LastSyncedAt = parseTimePtr(nullStringPtr(lastSync))
	a.CreatedAt = parseTime(created)
	return &a, nil
}

// CreateAccount 创建账号；ID 为空时自动生成，密码只以密文 BLOB 落库。
func (s *Store) CreateAccount(ctx context.Context, a *Account) error {
	if a == nil {
		return fmt.Errorf("%w: 账号不能为空", ErrInvalid)
	}
	if err := a.Validate(); err != nil {
		return err
	}
	if a.ID == "" {
		a.ID = NewID()
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	if a.Provider == "" {
		a.Provider = "imap"
	}
	if a.Status == "" {
		a.Status = AccountActive
	}
	if a.IMAPPort == 0 {
		a.IMAPPort = 993
	}
	if a.SMTPPort == 0 {
		a.SMTPPort = 465
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO accounts (id, provider, email, display_name, username, password_enc,
	imap_host, imap_port, imap_secure, smtp_host, smtp_port, smtp_secure, status, last_error, last_synced_at, created_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.Provider, a.Email, a.DisplayName, a.Username, a.PasswordEnc,
		a.IMAPHost, a.IMAPPort, boolInt(a.IMAPSecure), a.SMTPHost, a.SMTPPort, boolInt(a.SMTPSecure),
		a.Status, a.LastError, formatTimePtr(a.LastSyncedAt), formatTime(a.CreatedAt),
	)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: 邮箱 %s 已存在", ErrInvalid, a.Email)
		}
		return fmt.Errorf("store: 创建账号失败: %w", err)
	}
	return nil
}

// UpdateAccount 更新账号可修改字段；PasswordEnc 为空时保留原密文。
func (s *Store) UpdateAccount(ctx context.Context, a *Account) error {
	if a == nil || a.ID == "" {
		return fmt.Errorf("%w: 账号 ID 不能为空", ErrInvalid)
	}
	if a.Email == "" || a.Username == "" || a.IMAPHost == "" || a.SMTPHost == "" {
		return fmt.Errorf("%w: 账号必填字段缺失", ErrInvalid)
	}
	args := []any{a.Provider, a.Email, a.DisplayName, a.Username,
		a.IMAPHost, a.IMAPPort, boolInt(a.IMAPSecure),
		a.SMTPHost, a.SMTPPort, boolInt(a.SMTPSecure),
		a.Status, a.LastError, formatTimePtr(a.LastSyncedAt)}
	query := `UPDATE accounts SET provider = ?, email = ?, display_name = ?, username = ?,
	imap_host = ?, imap_port = ?, imap_secure = ?, smtp_host = ?, smtp_port = ?, smtp_secure = ?,
	status = ?, last_error = ?, last_synced_at = ?`
	if len(a.PasswordEnc) > 0 {
		query += ", password_enc = ?"
		args = append(args, a.PasswordEnc)
	}
	query += " WHERE id = ?"
	args = append(args, a.ID)

	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: 邮箱 %s 已存在", ErrInvalid, a.Email)
		}
		return fmt.Errorf("store: 更新账号失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: 更新账号失败: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: 账号 %s", ErrNotFound, a.ID)
	}
	return nil
}

// GetAccount 按 ID 查询账号。
func (s *Store) GetAccount(ctx context.Context, id string) (*Account, error) {
	if id == "" {
		return nil, fmt.Errorf("%w: 账号 ID 不能为空", ErrInvalid)
	}
	row := s.db.QueryRowContext(ctx, "SELECT "+accountColumns+" FROM accounts WHERE id = ?", id)
	a, err := scanAccount(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("%w: 账号 %s", ErrNotFound, id)
		}
		return nil, fmt.Errorf("store: 查询账号失败: %w", err)
	}
	return a, nil
}

// GetAccountByEmail 按邮箱查询账号。
func (s *Store) GetAccountByEmail(ctx context.Context, email string) (*Account, error) {
	if email == "" {
		return nil, fmt.Errorf("%w: 邮箱不能为空", ErrInvalid)
	}
	row := s.db.QueryRowContext(ctx, "SELECT "+accountColumns+" FROM accounts WHERE email = ? LIMIT 1", email)
	a, err := scanAccount(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("%w: 邮箱 %s", ErrNotFound, email)
		}
		return nil, fmt.Errorf("store: 查询账号失败: %w", err)
	}
	return a, nil
}

// ListAccounts 返回全部账号，按创建时间升序。
func (s *Store) ListAccounts(ctx context.Context) ([]*Account, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+accountColumns+" FROM accounts ORDER BY created_at ASC, id ASC")
	if err != nil {
		return nil, fmt.Errorf("store: 查询账号列表失败: %w", err)
	}
	defer rows.Close()

	out := make([]*Account, 0, 4)
	for rows.Next() {
		a, scanErr := scanAccount(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("store: 扫描账号失败: %w", scanErr)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历账号失败: %w", err)
	}
	return out, nil
}

// DeleteAccount 删除账号；文件夹、邮件等由外键级联删除。
func (s *Store) DeleteAccount(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("%w: 账号 ID 不能为空", ErrInvalid)
	}
	res, err := s.db.ExecContext(ctx, "DELETE FROM accounts WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("store: 删除账号失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: 删除账号失败: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: 账号 %s", ErrNotFound, id)
	}
	return nil
}

// SetAccountStatus 更新账号状态与最近错误。
func (s *Store) SetAccountStatus(ctx context.Context, id, status, lastError string) error {
	if id == "" {
		return fmt.Errorf("%w: 账号 ID 不能为空", ErrInvalid)
	}
	if status == "" {
		status = AccountActive
	}
	res, err := s.db.ExecContext(ctx, "UPDATE accounts SET status = ?, last_error = ? WHERE id = ?", status, lastError, id)
	if err != nil {
		return fmt.Errorf("store: 更新账号状态失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: 更新账号状态失败: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: 账号 %s", ErrNotFound, id)
	}
	return nil
}

// TouchAccount 记录账号最近一次成功同步时间。
func (s *Store) TouchAccount(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("%w: 账号 ID 不能为空", ErrInvalid)
	}
	res, err := s.db.ExecContext(ctx, "UPDATE accounts SET last_synced_at = ? WHERE id = ?", formatTime(time.Now().UTC()), id)
	if err != nil {
		return fmt.Errorf("store: 更新账号同步时间失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: 更新账号同步时间失败: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: 账号 %s", ErrNotFound, id)
	}
	return nil
}
