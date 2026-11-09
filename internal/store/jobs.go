package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const jobColumns = `id, kind, COALESCE(account_id, ''), payload, run_at, attempts, status, last_error, created_at, claimed_at`

func scanJob(sc rowScanner) (*Job, error) {
	var (
		j       Job
		runAt   string
		claimed sql.NullString
		created string
	)
	if err := sc.Scan(&j.ID, &j.Kind, &j.AccountID, &j.Payload, &runAt, &j.Attempts, &j.Status, &j.LastError, &created, &claimed); err != nil {
		return nil, err
	}
	if claimed.Valid {
		j.ClaimedAt = parseTime(claimed.String)
	}
	j.RunAt = parseTime(runAt)
	j.CreatedAt = parseTime(created)
	return &j, nil
}

// EnqueueJob 写入一个待执行任务。
func (s *Store) EnqueueJob(ctx context.Context, j *Job) error {
	if j == nil || j.Kind == "" {
		return fmt.Errorf("%w: 任务类型不能为空", ErrInvalid)
	}
	if j.ID == "" {
		j.ID = NewID()
	}
	if j.Payload == "" {
		j.Payload = "{}"
	}
	if j.Status == "" {
		j.Status = JobPending
	}
	now := time.Now().UTC()
	if j.RunAt.IsZero() {
		j.RunAt = now
	}
	if j.CreatedAt.IsZero() {
		j.CreatedAt = now
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO jobs (id, kind, account_id, payload, run_at, attempts, status, last_error, created_at, claimed_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)`,
		j.ID, j.Kind, nullIfEmpty(j.AccountID), j.Payload, formatTime(j.RunAt), j.Attempts,
		j.Status, j.LastError, formatTime(j.CreatedAt))
	if err != nil {
		return fmt.Errorf("store: 写入任务失败: %w", err)
	}
	return nil
}

// RecoverRunningJobs 恢复遗留 running 任务。lease 为零时恢复全部；正值仅恢复超时租约。
// retry_send 可能在远端已接收而客户端崩溃时重复投递，因此标为结果不确定并转人工审核。
func (s *Store) RecoverRunningJobs(ctx context.Context, lease time.Duration) error {
	if lease < 0 {
		lease = 0
	}
	now := time.Now().UTC()
	cutoff := formatTime(now.Add(-lease))
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET
	status = CASE WHEN kind IN ('retry_send', 'send_review') THEN 'review' ELSE 'pending' END,
	last_error = CASE WHEN kind IN ('retry_send', 'send_review') THEN '进程在发送任务执行期间中断，投递结果不确定；请核查邮箱后手动处理' ELSE last_error END,
	claimed_at = NULL,
	run_at = CASE WHEN kind IN ('retry_send', 'send_review') THEN run_at ELSE ? END
	WHERE status = 'running' AND (claimed_at IS NULL OR claimed_at <= ?)`, formatTime(now), cutoff)
	if err != nil {
		return fmt.Errorf("store: 恢复过期运行任务失败: %w", err)
	}
	return nil
}

// DueJobs 返回到期待执行的任务，按 run_at 升序。
func (s *Store) DueJobs(ctx context.Context, limit int) ([]*Job, error) {
	if limit <= 0 {
		limit = 10
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+jobColumns+" FROM jobs WHERE status = ? AND run_at <= ? ORDER BY run_at ASC, id ASC LIMIT ?",
		JobPending, formatTime(time.Now().UTC()), limit)
	if err != nil {
		return nil, fmt.Errorf("store: 查询到期任务失败: %w", err)
	}
	defer rows.Close()

	out := make([]*Job, 0, limit)
	for rows.Next() {
		j, scanErr := scanJob(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("store: 扫描任务失败: %w", scanErr)
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历任务失败: %w", err)
	}
	return out, nil
}

// HoldJobForReview 把执行结果不确定的任务留给人工核查，不再自动重试。
func (s *Store) HoldJobForReview(ctx context.Context, id, message string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("%w: 任务 ID 不能为空", ErrInvalid)
	}
	res, err := s.db.ExecContext(ctx, "UPDATE jobs SET status = ?, last_error = ?, claimed_at = NULL WHERE id = ?", JobReview, message, id)
	if err != nil {
		return fmt.Errorf("store: 标记待核查任务失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: 标记待核查任务失败: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: 任务 %s", ErrNotFound, id)
	}
	return nil
}

// ClaimJob 把待执行任务置为 running。
func (s *Store) ClaimJob(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("%w: 任务 ID 不能为空", ErrInvalid)
	}
	res, err := s.db.ExecContext(ctx, "UPDATE jobs SET status = ?, claimed_at = ? WHERE id = ? AND status = ?", JobRunning, formatTime(time.Now().UTC()), id, JobPending)
	if err != nil {
		return fmt.Errorf("store: 领取任务失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: 领取任务失败: %w", err)
	}
	if n == 0 {
		var exists int
		if qerr := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM jobs WHERE id = ?)", id).Scan(&exists); qerr != nil {
			return fmt.Errorf("store: 查询任务失败: %w", qerr)
		}
		if exists == 0 {
			return fmt.Errorf("%w: 任务 %s", ErrNotFound, id)
		}
		return fmt.Errorf("%w: 任务 %s 当前不可领取", ErrInvalid, id)
	}
	return nil
}

// FinishJob 结束任务：成功置 done；失败 attempts+1，达到 3 次置 failed，否则按 1min/5min 退避重试。
func (s *Store) FinishJob(ctx context.Context, id string, jobErr error) error {
	if id == "" {
		return fmt.Errorf("%w: 任务 ID 不能为空", ErrInvalid)
	}
	var res sql.Result
	var err error
	if jobErr == nil {
		res, err = s.db.ExecContext(ctx,
			"UPDATE jobs SET status = ?, last_error = '', claimed_at = NULL WHERE id = ?", JobDone, id)
	} else {
		now := time.Now().UTC()
		retry1 := formatTime(now.Add(time.Minute))
		retry2 := formatTime(now.Add(5 * time.Minute))
		res, err = s.db.ExecContext(ctx, `UPDATE jobs SET
	attempts = attempts + 1,
	status = CASE WHEN attempts + 1 >= 3 THEN 'failed' ELSE 'pending' END,
	last_error = ?,
	claimed_at = NULL,
	run_at = CASE
		WHEN attempts + 1 >= 3 THEN run_at
		WHEN attempts + 1 = 1 THEN ?
		WHEN attempts + 1 = 2 THEN ?
		ELSE run_at END
	WHERE id = ?`, jobErr.Error(), retry1, retry2, id)
	}
	if err != nil {
		return fmt.Errorf("store: 结束任务失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: 结束任务失败: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: 任务 %s", ErrNotFound, id)
	}
	return nil
}
