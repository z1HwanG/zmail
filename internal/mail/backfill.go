package mail

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/emersion/go-imap"

	"zmail/internal/store"
)

// 历史回填参数：低优先级、每批 50 封、完成后休眠；检测到实时任务时降速。
const (
	backfillBatch        = 50
	backfillInitialDelay = 10 * time.Second
	backfillIdleDelay    = 60 * time.Second
	backfillBusyDelay    = 3 * time.Minute
	backfillQuietDelay   = 5 * time.Minute
	backfillMaxAttempts  = 3
)

// backfillLoop 周期性选择最需要回填的文件夹，每次拉一小批并休眠。
func (m *Manager) backfillLoop(ctx context.Context) {
	if !sleepCtx(ctx, backfillInitialDelay) {
		return
	}
	quietPasses := 0
	for {
		if err := m.runBackfillStep(ctx); err != nil && ctx.Err() == nil {
			m.log.Warn("历史回填失败", "error", err)
		}
		quietPasses = m.updateBackfillQuiet(ctx, quietPasses)

		delay := backfillIdleDelay
		switch {
		case m.fetchBusy():
			// 有实时同步任务在跑：降速让路。
			delay = backfillBusyDelay
		case quietPasses >= 3:
			// 连续多轮没有新邮件：进一步拉长间隔。
			delay = backfillQuietDelay
		}
		if !sleepCtx(ctx, delay) {
			return
		}
	}
}

// updateBackfillQuiet 记录连续「没有新邮件」的轮次，用于自适应降速。
func (m *Manager) updateBackfillQuiet(_ context.Context, quiet int) int {
	m.syncMu.Lock()
	inserted := m.lastBackfill
	m.syncMu.Unlock()
	if inserted == 0 {
		return quiet + 1
	}
	return 0
}

// runBackfillStep 执行一次历史回填：选 priority 最高（数值最小）、最久未同步的启用文件夹，
// 拉取最多 50 封。不包含收件箱（收件箱走 IDLE + 实时同步）。
func (m *Manager) runBackfillStep(ctx context.Context) error {
	if m == nil || m.st == nil {
		return errManagerStopped
	}
	if m.cfg != nil && !m.cfg.BackfillEnabled {
		return nil
	}
	// 有实时任务时让路，避免抢占连接与 DB 写锁。
	if m.fetchBusy() {
		return nil
	}

	folders, err := m.st.ListFolders(ctx, "")
	if err != nil {
		return fmt.Errorf("mail: 读取文件夹列表失败: %w", err)
	}

	type candidate struct {
		folder *store.Folder
	}
	candidates := make([]candidate, 0, len(folders))
	for _, f := range folders {
		if f == nil || !f.Enabled {
			continue
		}
		// 收件箱同样需要历史回填（实时同步只覆盖最近 500 封），
		// 否则邮件多的账号永远看不到更早的信。
		candidates = append(candidates, candidate{folder: f})
	}
	if len(candidates) == 0 {
		return nil
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i].folder, candidates[j].folder
		// 尚未回填完的文件夹优先，其次按 priority、最后按最久未同步。
		aDone := a.BackfillUID < 0
		bDone := b.BackfillUID < 0
		if aDone != bDone {
			return !aDone
		}
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		// 从未同步的优先；否则最久未同步的优先。
		if a.LastSyncedAt == nil {
			return true
		}
		if b.LastSyncedAt == nil {
			return false
		}
		return a.LastSyncedAt.Before(*b.LastSyncedAt)
	})

	attempts := 0
	var firstErr error
	for _, c := range candidates {
		if attempts >= backfillMaxAttempts {
			break
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		acc, err := m.st.GetAccount(ctx, c.folder.AccountID)
		if err != nil {
			continue
		}
		if acc.Status != store.AccountActive {
			continue
		}
		attempts++
		inserted, err := m.syncFolderLimited(ctx, acc, c.folder, backfillBatch)
		if err != nil {
			if isAuthError(err) {
				m.markAuthFailed(ctx, acc, err)
			}
			if firstErr == nil {
				firstErr = err
			}
			m.log.Warn("回填文件夹失败", "account", acc.ID, "folder", c.folder.RemotePath, "error", err)
			continue
		}
		// 增量同步之后再向更早的 UID 方向回填一批。
		older, oerr := m.syncFolderOlderLimited(ctx, acc, c.folder, backfillBatch)
		if oerr != nil {
			if isAuthError(oerr) {
				m.markAuthFailed(ctx, acc, oerr)
			}
			if firstErr == nil {
				firstErr = oerr
			}
			m.log.Warn("历史回填失败", "account", acc.ID, "folder", c.folder.RemotePath, "error", oerr)
		}
		m.setLastBackfill(inserted + older)
		return nil
	}
	return firstErr
}

// syncFolderLimited 在账号锁 + 全局并发槽内回填单个文件夹。
func (m *Manager) syncFolderLimited(ctx context.Context, acc *store.Account, f *store.Folder, limit int) (int, error) {
	unlock := m.accountLock(acc.ID)
	defer unlock()
	release, err := m.acquireFetch(ctx)
	if err != nil {
		return 0, err
	}
	defer release()
	return m.syncFolder(ctx, acc, f, limit)
}

// SyncFolderOlder 供 HTTP 接口调用：手动为某个文件夹拉取一批更早的邮件。
func (m *Manager) SyncFolderOlder(ctx context.Context, folderID string, limit int) (int, error) {
	if m == nil || m.st == nil {
		return 0, errManagerStopped
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	f, err := m.st.GetFolder(ctx, folderID)
	if err != nil {
		return 0, err
	}
	acc, err := m.st.GetAccount(ctx, f.AccountID)
	if err != nil {
		return 0, err
	}
	if acc.Status == store.AccountDisabled {
		return 0, errAccountDisabled
	}
	return m.syncFolderOlderLimited(ctx, acc, f, limit)
}

// syncFolderOlderLimited 在账号锁 + 全局并发槽内回填更早的邮件。
func (m *Manager) syncFolderOlderLimited(ctx context.Context, acc *store.Account, f *store.Folder, limit int) (int, error) {
	unlock := m.accountLock(acc.ID)
	defer unlock()
	release, err := m.acquireFetch(ctx)
	if err != nil {
		return 0, err
	}
	defer release()
	return m.syncFolderOlder(ctx, acc, f, limit)
}

// syncFolderOlder 从 backfill_uid 向下拉取一批更早的邮件（每次最多 limit 封）。
// backfill_uid 语义：0=未开始（以本地最老 UID 初始化），-1=已完成，>0=下一次的上界（不含）。
func (m *Manager) syncFolderOlder(ctx context.Context, acc *store.Account, f *store.Folder, limit int) (int, error) {
	if acc == nil || f == nil {
		return 0, fmt.Errorf("%w: 账号或文件夹为空", errInvalidAccount)
	}
	if f.BackfillUID < 0 {
		return 0, nil
	}
	if limit <= 0 {
		limit = backfillBatch
	}

	conn, err := m.openIMAP(ctx, acc, imapDialTimeout)
	if err != nil {
		return 0, err
	}
	defer conn.close()
	conn.client.Timeout = imapOpTimeout

	mb, err := conn.client.Select(f.RemotePath, true)
	if err != nil {
		return 0, fmt.Errorf("mail: 打开文件夹 %q 失败: %w", f.RemotePath, err)
	}
	// UIDVALIDITY 已变化：等实时同步重置后再回填，避免写入旧 UID。
	if f.UIDValidity != 0 && mb.UidValidity != f.UIDValidity {
		return 0, nil
	}

	cursor := f.BackfillUID
	if cursor == 0 {
		if minUID, ok, err := m.st.MinRemoteUID(ctx, f.ID); err != nil {
			return 0, err
		} else if ok {
			cursor = int(minUID) - 1
		} else if mb.UidNext > 0 {
			cursor = int(mb.UidNext) - 1
		}
	}
	if cursor <= 0 {
		if err := m.st.UpdateFolderBackfill(ctx, f.ID, -1); err != nil {
			return 0, err
		}
		f.BackfillUID = -1
		return 0, nil
	}

	seq := &imap.SeqSet{}
	seq.AddRange(1, uint32(cursor))
	uids, err := conn.client.UidSearch(&imap.SearchCriteria{Uid: seq})
	if err != nil {
		return 0, fmt.Errorf("mail: UID SEARCH 历史邮件 %q 失败: %w", f.RemotePath, err)
	}
	if len(uids) == 0 {
		if err := m.st.UpdateFolderBackfill(ctx, f.ID, -1); err != nil {
			return 0, err
		}
		f.BackfillUID = -1
		return 0, nil
	}
	sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })

	// 取最大的 limit 个 UID（最接近当前时间的一批更早邮件）。
	start := 0
	if len(uids) > limit {
		start = len(uids) - limit
	}
	batch := uids[start:]

	inserted := 0
	items, ferr := m.fetchBatch(ctx, conn, acc, f, mb, batch)
	if ferr != nil {
		return 0, ferr
	}
	if len(items) > 0 {
		bundles := make([]*store.MessageBundle, 0, len(items))
		for _, it := range items {
			bundles = append(bundles, it.bundle)
		}
		n, ierr := m.st.InsertMessages(ctx, bundles)
		if ierr != nil {
			return 0, fmt.Errorf("mail: 写入历史邮件失败: %w", ierr)
		}
		inserted = n
	}

	next := int(batch[0]) - 1
	if next <= 0 {
		next = -1
	}
	if err := m.st.UpdateFolderBackfill(ctx, f.ID, next); err != nil {
		return inserted, err
	}
	f.BackfillUID = next

	// 顺带刷新文件夹计数，保持侧栏未读数准确。
	if mb != nil {
		_ = m.st.UpdateFolderCounts(ctx, f.ID, int(mb.Unseen), int(mb.Messages))
	}
	if inserted > 0 {
		m.log.Info("历史回填完成一批", "account", acc.ID, "folder", f.RemotePath, "inserted", inserted, "next", next)
	}
	return inserted, nil
}

// setLastBackfill 记录最近一次回填插入数。
func (m *Manager) setLastBackfill(n int) {
	m.syncMu.Lock()
	m.lastBackfill = n
	m.syncMu.Unlock()
}
