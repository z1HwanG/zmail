package mail

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/emersion/go-imap"

	"zmail/internal/store"
)

// deleteRemotePayload 是 kind="delete_remote" 任务的 payload。
type deleteRemotePayload struct {
	MessageID string `json:"messageId"`
	AccountID string `json:"accountId"`
	FolderID  string `json:"folderId"`
	RemoteUID uint32 `json:"remoteUid"`
	Action    string `json:"action"`
}

// runDeleteRemote 执行一条「删除远端邮件」任务：
// 解析账号/文件夹 → SELECT → UID STORE +FLAGS Deleted → EXPUNGE。
// 失败时写 sync log 并返回错误，由 store.FinishJob 按重试策略处理。
func (m *Manager) runDeleteRemote(ctx context.Context, job *store.Job) error {
	if job == nil || job.Payload == "" {
		return fmt.Errorf("mail: 删除任务缺少 payload")
	}
	var payload deleteRemotePayload
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
		return fmt.Errorf("mail: 解析删除任务失败: %w", err)
	}
	if payload.AccountID == "" {
		payload.AccountID = job.AccountID
	}
	if payload.Action == "" {
		payload.Action = "delete"
	}
	accountID := payload.AccountID
	if accountID == "" {
		return fmt.Errorf("mail: 删除任务缺少账号 ID")
	}

	fail := func(err error) error {
		m.syncLog(ctx, accountID, "error", "远程删除邮件失败："+err.Error())
		return err
	}

	if payload.Action != "delete" {
		return fail(fmt.Errorf("mail: 不支持的删除动作 %q", payload.Action))
	}
	if payload.FolderID == "" || payload.RemoteUID == 0 {
		return fail(fmt.Errorf("mail: 删除任务缺少文件夹 ID 或远端 UID"))
	}
	folder, err := m.st.GetFolder(ctx, payload.FolderID)
	if err != nil {
		return fail(fmt.Errorf("mail: 读取文件夹失败: %w", err))
	}
	if payload.AccountID != "" && folder.AccountID != payload.AccountID {
		return fail(fmt.Errorf("mail: 文件夹 %s 不属于账号 %s", folder.ID, payload.AccountID))
	}
	acc, err := m.st.GetAccount(ctx, folder.AccountID)
	if err != nil {
		return fail(fmt.Errorf("mail: 读取账号失败: %w", err))
	}

	conn, err := m.openIMAP(ctx, acc, imapDialTimeout)
	if err != nil {
		return fail(err)
	}
	defer conn.close()
	conn.client.Timeout = imapOpTimeout

	if _, err := conn.client.Select(folder.RemotePath, false); err != nil {
		return fail(fmt.Errorf("mail: 打开文件夹 %q 失败: %w", folder.RemotePath, err))
	}

	seq := &imap.SeqSet{}
	seq.AddNum(payload.RemoteUID)
	item := imap.FormatFlagsOp(imap.AddFlags, true)
	if err := conn.client.UidStore(seq, item, []interface{}{imap.DeletedFlag}, nil); err != nil {
		return fail(fmt.Errorf("mail: 标记邮件为已删除状态失败: %w", err))
	}
	// go-imap v1.2.1 没有 UID EXPUNGE，退化为 EXPUNGE（仅清理已标记 Deleted 的邮件）。
	if err := conn.client.Expunge(nil); err != nil {
		return fail(fmt.Errorf("mail: EXPUNGE 失败: %w", err))
	}

	m.syncLog(ctx, accountID, "info", fmt.Sprintf("已从 IMAP 删除邮件（folder=%s uid=%d）", folder.RemotePath, payload.RemoteUID))
	return nil
}
