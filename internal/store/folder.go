package store

import (
	"context"
	"database/sql"
	"fmt"
)

const folderColumns = `id, account_id, remote_path, name, role, uid_validity, last_uid,
	unread_count, total_count, enabled, priority, last_synced_at, backfill_uid`

func scanFolder(sc rowScanner) (*Folder, error) {
	var (
		f        Folder
		uidValid int64
		lastUID  int64
		enabled  int
		lastSync sql.NullString
	)
	var backfillUID int
	if err := sc.Scan(
		&f.ID, &f.AccountID, &f.RemotePath, &f.Name, &f.Role,
		&uidValid, &lastUID, &f.UnreadCount, &f.TotalCount, &enabled, &f.Priority, &lastSync, &backfillUID,
	); err != nil {
		return nil, err
	}
	f.UIDValidity = uint32(uidValid)
	f.LastUID = uint32(lastUID)
	f.Enabled = enabled != 0
	f.LastSyncedAt = parseTimePtr(nullStringPtr(lastSync))
	f.BackfillUID = backfillUID
	return &f, nil
}

// UpsertFolder 按 (account_id, remote_path) 幂等写入文件夹。
// 已存在时只更新名称/角色/启用状态/优先级，保留 uid_validity 与 last_uid 游标。
func (s *Store) UpsertFolder(ctx context.Context, f *Folder) error {
	if f == nil {
		return fmt.Errorf("%w: 文件夹不能为空", ErrInvalid)
	}
	if f.AccountID == "" || f.RemotePath == "" {
		return fmt.Errorf("%w: 账号 ID 与远程路径不能为空", ErrInvalid)
	}
	if f.ID == "" {
		f.ID = NewID()
	}
	if f.Name == "" {
		f.Name = f.RemotePath
	}
	if f.Role == "" {
		f.Role = RoleCustom
	}
	if f.Priority == 0 {
		f.Priority = 100
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: 开启事务失败: %w", err)
	}
	defer tx.Rollback()

	// 每个账号最多一个 inbox：远端路径变化时先把旧的 inbox 降级，避免唯一索引冲突。
	if f.Role == RoleInbox {
		if _, err := tx.ExecContext(ctx,
			"UPDATE folders SET role = ? WHERE account_id = ? AND role = ? AND remote_path <> ?",
			RoleCustom, f.AccountID, RoleInbox, f.RemotePath); err != nil {
			return fmt.Errorf("store: 调整 inbox 文件夹失败: %w", err)
		}
	}

	_, err = tx.ExecContext(ctx, `INSERT INTO folders
	(id, account_id, remote_path, name, role, uid_validity, last_uid, unread_count, total_count, enabled, priority, last_synced_at, backfill_uid)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(account_id, remote_path) DO UPDATE SET
		name = excluded.name,
		role = excluded.role,
		enabled = excluded.enabled,
		priority = excluded.priority,
		last_synced_at = excluded.last_synced_at`,
		f.ID, f.AccountID, f.RemotePath, f.Name, f.Role, int64(f.UIDValidity), int64(f.LastUID),
		f.UnreadCount, f.TotalCount, boolInt(f.Enabled), f.Priority, formatTimePtr(f.LastSyncedAt), f.BackfillUID,
	)
	if err != nil {
		return fmt.Errorf("store: 写入文件夹失败: %w", err)
	}

	// 回填真实主键与既有游标。
	if err := tx.QueryRowContext(ctx,
		"SELECT id, uid_validity, last_uid, backfill_uid FROM folders WHERE account_id = ? AND remote_path = ?",
		f.AccountID, f.RemotePath).Scan(&f.ID, &f.UIDValidity, &f.LastUID, &f.BackfillUID); err != nil {
		return fmt.Errorf("store: 读取文件夹主键失败: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: 提交文件夹事务失败: %w", err)
	}
	return nil
}

// GetFolder 按 ID 查询文件夹。
func (s *Store) GetFolder(ctx context.Context, id string) (*Folder, error) {
	if id == "" {
		return nil, fmt.Errorf("%w: 文件夹 ID 不能为空", ErrInvalid)
	}
	row := s.db.QueryRowContext(ctx, "SELECT "+folderColumns+" FROM folders WHERE id = ?", id)
	f, err := scanFolder(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("%w: 文件夹 %s", ErrNotFound, id)
		}
		return nil, fmt.Errorf("store: 查询文件夹失败: %w", err)
	}
	return f, nil
}

// GetFolderByRemotePath 按账号与远端路径查询文件夹。
func (s *Store) GetFolderByRemotePath(ctx context.Context, accountID, remotePath string) (*Folder, error) {
	if accountID == "" || remotePath == "" {
		return nil, fmt.Errorf("%w: 账号 ID 与远程路径不能为空", ErrInvalid)
	}
	row := s.db.QueryRowContext(ctx,
		"SELECT "+folderColumns+" FROM folders WHERE account_id = ? AND remote_path = ?", accountID, remotePath)
	f, err := scanFolder(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("%w: 文件夹 %s", ErrNotFound, remotePath)
		}
		return nil, fmt.Errorf("store: 查询文件夹失败: %w", err)
	}
	return f, nil
}

// GetFolderByRole 按账号与角色查询文件夹。
func (s *Store) GetFolderByRole(ctx context.Context, accountID, role string) (*Folder, error) {
	if accountID == "" || role == "" {
		return nil, fmt.Errorf("%w: 账号 ID 与角色不能为空", ErrInvalid)
	}
	row := s.db.QueryRowContext(ctx,
		"SELECT "+folderColumns+" FROM folders WHERE account_id = ? AND role = ? ORDER BY priority ASC, id ASC LIMIT 1",
		accountID, role)
	f, err := scanFolder(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("%w: 账号 %s 的角色 %s", ErrNotFound, accountID, role)
		}
		return nil, fmt.Errorf("store: 查询文件夹失败: %w", err)
	}
	return f, nil
}

// ListFolders 列出文件夹；accountID 为空时返回全部。
func (s *Store) ListFolders(ctx context.Context, accountID string) ([]*Folder, error) {
	query := "SELECT " + folderColumns + " FROM folders"
	args := []any{}
	if accountID != "" {
		query += " WHERE account_id = ?"
		args = append(args, accountID)
	}
	query += " ORDER BY account_id ASC, priority ASC, name ASC"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 查询文件夹列表失败: %w", err)
	}
	defer rows.Close()

	out := make([]*Folder, 0, 8)
	for rows.Next() {
		f, scanErr := scanFolder(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("store: 扫描文件夹失败: %w", scanErr)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历文件夹失败: %w", err)
	}
	return out, nil
}

// UpdateFolderCursor 更新 UIDVALIDITY 与 last_uid 游标。
func (s *Store) UpdateFolderCursor(ctx context.Context, folderID string, uidValidity, lastUID uint32) error {
	if folderID == "" {
		return fmt.Errorf("%w: 文件夹 ID 不能为空", ErrInvalid)
	}
	res, err := s.db.ExecContext(ctx,
		"UPDATE folders SET uid_validity = ?, last_uid = ? WHERE id = ?",
		int64(uidValidity), int64(lastUID), folderID)
	if err != nil {
		return fmt.Errorf("store: 更新文件夹游标失败: %w", err)
	}
	return folderRowsAffected(res, folderID)
}

// UpdateFolderCounts 更新未读数与总数。
func (s *Store) UpdateFolderCounts(ctx context.Context, folderID string, unread, total int) error {
	if folderID == "" {
		return fmt.Errorf("%w: 文件夹 ID 不能为空", ErrInvalid)
	}
	res, err := s.db.ExecContext(ctx,
		"UPDATE folders SET unread_count = ?, total_count = ? WHERE id = ?", unread, total, folderID)
	if err != nil {
		return fmt.Errorf("store: 更新文件夹计数失败: %w", err)
	}
	return folderRowsAffected(res, folderID)
}

func folderRowsAffected(res sql.Result, folderID string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: 读取受影响行数失败: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: 文件夹 %s", ErrNotFound, folderID)
	}
	return nil
}

// ResetFolderMessages 清空文件夹邮件并重置 UIDVALIDITY 与 last_uid（UIDVALIDITY 变化时使用）。
func (s *Store) ResetFolderMessages(ctx context.Context, folderID string, uidValidity uint32) error {
	if folderID == "" {
		return fmt.Errorf("%w: 文件夹 ID 不能为空", ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: 开启事务失败: %w", err)
	}
	defer tx.Rollback()

	var accountID string
	if err := tx.QueryRowContext(ctx, "SELECT account_id FROM folders WHERE id = ?", folderID).Scan(&accountID); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("%w: 文件夹 %s", ErrNotFound, folderID)
		}
		return fmt.Errorf("store: 查询文件夹失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM messages WHERE folder_id = ?", folderID); err != nil {
		return fmt.Errorf("store: 清空文件夹邮件失败: %w", err)
	}
	// 清理该账号下已经没有邮件的会话，避免 thread 计数残留。
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM threads WHERE account_id = ? AND id NOT IN (SELECT thread_id FROM messages WHERE thread_id IS NOT NULL)",
		accountID); err != nil {
		return fmt.Errorf("store: 清理会话失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE folders SET uid_validity = ?, last_uid = 0 WHERE id = ?", int64(uidValidity), folderID); err != nil {
		return fmt.Errorf("store: 重置文件夹游标失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: 提交重置事务失败: %w", err)
	}
	return nil
}

// SetFolderEnabled 启用或停用文件夹同步。
func (s *Store) SetFolderEnabled(ctx context.Context, folderID string, enabled bool) error {
	if folderID == "" {
		return fmt.Errorf("%w: 文件夹 ID 不能为空", ErrInvalid)
	}
	res, err := s.db.ExecContext(ctx, "UPDATE folders SET enabled = ? WHERE id = ?", boolInt(enabled), folderID)
	if err != nil {
		return fmt.Errorf("store: 更新文件夹启用状态失败: %w", err)
	}
	return folderRowsAffected(res, folderID)
}

// UpdateFolderBackfill 更新历史回填游标（0 未开始，-1 已完成，>0 下次从该 UID 向下继续）。
func (s *Store) UpdateFolderBackfill(ctx context.Context, folderID string, backfillUID int) error {
	if folderID == "" {
		return fmt.Errorf("%w: 文件夹 ID 不能为空", ErrInvalid)
	}
	res, err := s.db.ExecContext(ctx,
		"UPDATE folders SET backfill_uid = ? WHERE id = ?", backfillUID, folderID)
	if err != nil {
		return fmt.Errorf("store: 更新回填游标失败: %w", err)
	}
	return folderRowsAffected(res, folderID)
}

// MinRemoteUID 返回文件夹内已入库邮件的最小 UID；文件夹为空时 ok=false。
func (s *Store) MinRemoteUID(ctx context.Context, folderID string) (uint32, bool, error) {
	if folderID == "" {
		return 0, false, fmt.Errorf("%w: 文件夹 ID 不能为空", ErrInvalid)
	}
	var minUID sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		"SELECT MIN(remote_uid) FROM messages WHERE folder_id = ? AND is_deleted = 0", folderID).Scan(&minUID); err != nil {
		return 0, false, fmt.Errorf("store: 查询最老 UID 失败: %w", err)
	}
	if !minUID.Valid || minUID.Int64 <= 0 {
		return 0, false, nil
	}
	return uint32(minUID.Int64), true, nil
}

// CountFolderMessages 返回文件夹内未删除邮件数。
func (s *Store) CountFolderMessages(ctx context.Context, folderID string) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM messages WHERE folder_id = ? AND is_deleted = 0", folderID).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: 统计文件夹邮件失败: %w", err)
	}
	return n, nil
}

// DeleteFolder 删除文件夹；其下邮件与附件由外键级联删除。
func (s *Store) DeleteFolder(ctx context.Context, folderID string) error {
	if folderID == "" {
		return fmt.Errorf("%w: 文件夹 ID 不能为空", ErrInvalid)
	}
	res, err := s.db.ExecContext(ctx, "DELETE FROM folders WHERE id = ?", folderID)
	if err != nil {
		return fmt.Errorf("store: 删除文件夹失败: %w", err)
	}
	return folderRowsAffected(res, folderID)
}
