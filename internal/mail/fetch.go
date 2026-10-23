package mail

import (
	"context"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/emersion/go-imap"

	"zmail/internal/realtime"
	"zmail/internal/store"
	"zmail/internal/verify"
)

// firstSyncLimit 是首次同步最多回拉的最近邮件数。
const firstSyncLimit uint32 = 500

// fetchedMessage 是单封邮件的抓取结果（原始邮件 + 解析结果 + 待入库 bundle）。
type fetchedMessage struct {
	bundle *store.MessageBundle
	parsed *ParsedMessage
}

// syncFolder 同步单个文件夹：处理 UIDVALIDITY 变化、首次同步限流、增量拉取、
// 分批写入并在每批事务成功后推进 last_uid。limit > 0 时本次最多处理 limit 个 UID（回填用）。
func (m *Manager) syncFolder(ctx context.Context, acc *store.Account, f *store.Folder, limit int) (int, error) {
	if acc == nil || f == nil {
		return 0, fmt.Errorf("%w: 账号或文件夹为空", errInvalidAccount)
	}
	conn, err := m.openIMAP(ctx, acc, imapDialTimeout)
	if err != nil {
		return 0, err
	}
	defer conn.close()
	conn.client.Timeout = imapOpTimeout

	mb, err := conn.client.Select(f.RemotePath, false)
	if err != nil {
		return 0, fmt.Errorf("mail: 打开文件夹 %q 失败: %w", f.RemotePath, err)
	}

	// UIDVALIDITY 变化说明服务端 UID 已全部重置，清空本地后走首次同步。
	if f.UIDValidity != 0 && mb.UidValidity != f.UIDValidity {
		m.log.Info("UIDVALIDITY 变化，重置文件夹",
			"account", acc.ID, "folder", f.RemotePath, "old", f.UIDValidity, "new", mb.UidValidity)
		if err := m.st.ResetFolderMessages(ctx, f.ID, mb.UidValidity); err != nil {
			return 0, fmt.Errorf("mail: 重置文件夹 %q 失败: %w", f.RemotePath, err)
		}
		f.LastUID = 0
		f.UIDValidity = mb.UidValidity
		m.syncLog(ctx, acc.ID, "warn", "UIDVALIDITY 变化，已重置文件夹 "+f.RemotePath)
	}

	// 首次同步只拉最近 500 封（UIDNEXT-500）。
	startUID := f.LastUID
	if startUID == 0 && mb.UidNext > firstSyncLimit {
		startUID = mb.UidNext - firstSyncLimit
	}
	criteria := &imap.SearchCriteria{Uid: &imap.SeqSet{}}
	criteria.Uid.AddRange(startUID+1, 0)

	uids, err := conn.client.UidSearch(criteria)
	if err != nil {
		return 0, fmt.Errorf("mail: UID SEARCH %q 失败: %w", f.RemotePath, err)
	}
	sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })

	inserted := 0
	maxUID := f.LastUID
	remaining := limit

	for start := 0; start < len(uids); {
		end := start + imapFetchBatch
		if end > len(uids) {
			end = len(uids)
		}
		if remaining > 0 && end-start > remaining {
			end = start + remaining
		}
		if end <= start {
			break
		}
		batch := uids[start:end]

		items, ferr := m.fetchBatch(ctx, conn, acc, f, mb, batch)
		if ferr != nil {
			return inserted, ferr
		}
		if len(items) > 0 {
			bundles := make([]*store.MessageBundle, 0, len(items))
			for _, it := range items {
				bundles = append(bundles, it.bundle)
			}
			n, ierr := m.st.InsertMessages(ctx, bundles)
			if ierr != nil {
				// 事务回滚：游标不推进，稍后重试。
				return inserted, fmt.Errorf("mail: 写入文件夹 %q 的邮件失败: %w", f.RemotePath, ierr)
			}
			inserted += n
			for _, it := range items {
				// store.InsertMessages 只在真正插入时回填 ThreadID。
				if it.bundle.Message.ThreadID == "" {
					continue
				}
				m.announceMessage(ctx, acc, f, it)
			}
		}

		// 事务成功后才推进 last_uid。
		batchMax := batch[len(batch)-1]
		if batchMax > maxUID {
			maxUID = batchMax
		}
		if err := m.st.UpdateFolderCursor(ctx, f.ID, mb.UidValidity, maxUID); err != nil {
			return inserted, fmt.Errorf("mail: 更新文件夹 %q 游标失败: %w", f.RemotePath, err)
		}

		start = end
		if remaining > 0 {
			remaining -= len(batch)
			if remaining <= 0 {
				break
			}
		}
	}

	unread := int(mb.Unseen)
	total := int(mb.Messages)
	if err := m.st.UpdateFolderCounts(ctx, f.ID, unread, total); err != nil {
		m.log.Warn("更新文件夹计数失败", "account", acc.ID, "folder", f.RemotePath, "error", err)
	}
	// 记录文件夹最近同步时间（UpsertFolder 保留既有游标）。
	now := time.Now().UTC()
	fCopy := *f
	fCopy.LastSyncedAt = &now
	fCopy.UnreadCount = unread
	fCopy.TotalCount = total
	if err := m.st.UpsertFolder(ctx, &fCopy); err != nil {
		m.log.Warn("记录文件夹同步时间失败", "account", acc.ID, "folder", f.RemotePath, "error", err)
	}
	return inserted, nil
}

// fetchBatch 拉取一批 UID 的完整邮件（BODY.PEEK[] 不置已读）与 FLAGS。
func (m *Manager) fetchBatch(ctx context.Context, conn *imapConn, acc *store.Account, f *store.Folder, mb *imap.MailboxStatus, uids []uint32) ([]*fetchedMessage, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	section := &imap.BodySectionName{Peek: true}
	items := []imap.FetchItem{
		imap.FetchUid,
		imap.FetchFlags,
		imap.FetchInternalDate,
		imap.FetchRFC822Size,
		section.FetchItem(),
	}
	set := &imap.SeqSet{}
	set.AddNum(uids...)

	ch := make(chan *imap.Message, len(uids))
	errCh := make(chan error, 1)
	go func() {
		errCh <- conn.client.UidFetch(set, items, ch)
	}()

	out := make([]*fetchedMessage, 0, len(uids))
	for msg := range ch {
		if msg == nil {
			continue
		}
		lit := msg.GetBody(section)
		if lit == nil {
			m.log.Debug("邮件缺少 BODY[]，跳过", "account", acc.ID, "folder", f.RemotePath, "uid", msg.Uid)
			continue
		}
		raw, rerr := io.ReadAll(io.LimitReader(lit, maxRawMessageBytes))
		if rerr != nil {
			m.log.Warn("读取邮件原始内容失败，跳过", "account", acc.ID, "uid", msg.Uid, "error", rerr)
			continue
		}
		pm, perr := ParseBytes(raw)
		if perr != nil {
			// 单封解析失败只记日志跳过，不影响整批。
			m.log.Warn("解析邮件失败，跳过", "account", acc.ID, "folder", f.RemotePath, "uid", msg.Uid, "error", perr)
			continue
		}
		out = append(out, &fetchedMessage{
			bundle: m.buildBundle(acc, f, msg, pm),
			parsed: pm,
		})
	}
	if err := <-errCh; err != nil {
		return out, fmt.Errorf("mail: UID FETCH %q 失败: %w", f.RemotePath, err)
	}
	return out, nil
}

// buildBundle 把 IMAP 抓取结果与解析结果转换成入库结构。
func (m *Manager) buildBundle(acc *store.Account, f *store.Folder, im *imap.Message, pm *ParsedMessage) *store.MessageBundle {
	receivedAt := im.InternalDate
	if receivedAt.IsZero() {
		receivedAt = pm.Date
	}
	if receivedAt.IsZero() {
		receivedAt = time.Now().UTC()
	}

	var sentAt *time.Time
	if !pm.dateMissing && !pm.Date.IsZero() {
		t := pm.Date.UTC()
		sentAt = &t
	}

	msg := &store.Message{
		ID:             store.NewID(),
		AccountID:      acc.ID,
		FolderID:       f.ID,
		RemoteUID:      im.Uid,
		MessageID:      pm.MessageID,
		Subject:        pm.Subject,
		FromName:       pm.FromName,
		FromEmail:      pm.FromEmail,
		To:             pm.To,
		Cc:             pm.Cc,
		Bcc:            pm.Bcc,
		SentAt:         sentAt,
		ReceivedAt:     receivedAt.UTC(),
		Snippet:        pm.Snippet,
		BodyText:       pm.TextBody,
		BodyHTML:       pm.HTMLBody,
		HasAttachments: len(pm.Attachments) > 0,
		IsRead:         hasFlag(im.Flags, imap.SeenFlag),
		IsStarred:      hasFlag(im.Flags, imap.FlaggedFlag),
		IsDraft:        hasFlag(im.Flags, imap.DraftFlag),
		SizeBytes:      int64(len(pm.Raw)),
		CreatedAt:      time.Now().UTC(),
	}
	return &store.MessageBundle{
		Message:     msg,
		Attachments: pm.Attachments,
		Raw:         pm.Raw,
	}
}

// announceMessage 广播新邮件，并对收件箱最近 24 小时的邮件做验证码识别。
func (m *Manager) announceMessage(ctx context.Context, acc *store.Account, f *store.Folder, it *fetchedMessage) {
	msg := it.bundle.Message
	code := ""
	if f.Role == store.RoleInbox && time.Since(msg.ReceivedAt) <= 24*time.Hour {
		body := it.parsed.TextBody
		if body == "" {
			body = it.parsed.Snippet
		}
		code = verify.Extract(msg.Subject, body)
	}

	m.publish(realtime.EventMailNew, realtime.MailNew{
		ID:           msg.ID,
		AccountID:    acc.ID,
		AccountEmail: acc.Email,
		FolderID:     f.ID,
		Subject:      msg.Subject,
		FromName:     msg.FromName,
		FromEmail:    msg.FromEmail,
		Snippet:      msg.Snippet,
		ReceivedAt:   msg.ReceivedAt.UTC().Format(time.RFC3339),
		IsRead:       msg.IsRead,
		IsStarred:    msg.IsStarred,
		Code:         code,
	})

	if code != "" {
		from := msg.FromName
		if from == "" {
			from = msg.FromEmail
		}
		m.publish(realtime.EventMailCode, realtime.MailCode{
			MessageID: msg.ID,
			Code:      code,
			From:      from,
			Subject:   msg.Subject,
		})
		m.syncLog(ctx, acc.ID, "info", "识别到验证码邮件："+msg.Subject)
	}
	if rt := m.runtime(acc.ID); rt != nil {
		rt.touch()
	}
}
