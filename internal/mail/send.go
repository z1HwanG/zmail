package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/mail"
	"net/textproto"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	gomail "github.com/wneessen/go-mail"

	"zmail/internal/realtime"
	"zmail/internal/store"
)

// smtpTimeout 是 SMTP 连接与发送的命令超时。
const smtpTimeout = 45 * time.Second

// Send 发送一封邮件：SMTP 投递成功后 APPEND 到 Sent 文件夹并写入本地记录。
// 发送失败时写入 retry_send 任务（最多重试 3 次）并返回错误。
func (m *Manager) Send(ctx context.Context, req SendRequest) (*store.Message, error) {
	return m.send(ctx, req, true)
}

// send 是 Send 的内部实现；enqueueRetry=false 用于重试任务自身，避免无限入队。
func (m *Manager) send(ctx context.Context, req SendRequest, enqueueRetry bool) (*store.Message, error) {
	if m == nil || m.st == nil {
		return nil, errManagerStopped
	}
	if req.AccountID == "" {
		return nil, fmt.Errorf("%w: 账号 ID 为空", errInvalidAccount)
	}
	if len(req.To) == 0 && len(req.Cc) == 0 && len(req.Bcc) == 0 {
		return nil, fmt.Errorf("mail: 收件人不能为空")
	}
	if strings.TrimSpace(req.Subject) == "" && strings.TrimSpace(req.TextBody) == "" && strings.TrimSpace(req.HTMLBody) == "" {
		return nil, fmt.Errorf("mail: 主题和正文不能同时为空")
	}

	acc, err := m.st.GetAccount(ctx, req.AccountID)
	if err != nil {
		return nil, err
	}
	if acc.Status == store.AccountDisabled {
		return nil, fmt.Errorf("%w: 账号 %s", errAccountDisabled, req.AccountID)
	}
	if err := m.validateIMAPHost(ctx, acc.SMTPHost); err != nil {
		return nil, err
	}

	msg, raw, messageID, err := buildOutgoingMessage(acc, req)
	if err != nil {
		return nil, err
	}

	if err := m.deliverSMTP(ctx, acc, msg); err != nil {
		m.log.Warn("SMTP 发信失败", "account", acc.ID, "error", err)
		if enqueueRetry {
			m.enqueueSendRetry(ctx, acc.ID, req, err)
		}
		return nil, fmt.Errorf("mail: 发送邮件失败: %w", err)
	}

	// 发送成功：APPEND 到 Sent（若存在）并落本地记录。
	folderID, remoteUID, aerr := m.appendToSent(ctx, acc, raw, messageID)
	if aerr != nil {
		// APPEND 失败不影响「已发送」这一事实，只记录告警。
		m.log.Warn("APPEND 到已发送文件夹失败", "account", acc.ID, "error", aerr)
		m.syncLog(ctx, acc.ID, "warn", "APPEND 到已发送文件夹失败")
	}

	stored := m.insertSentMessage(ctx, acc, req, raw, messageID, folderID, remoteUID)
	if stored == nil {
		return nil, fmt.Errorf("mail: 邮件已发送，但本地保存失败")
	}

	m.publish(realtime.EventMailNew, realtime.MailNew{
		ID:           stored.ID,
		AccountID:    acc.ID,
		AccountEmail: acc.Email,
		FolderID:     stored.FolderID,
		Subject:      stored.Subject,
		FromName:     stored.FromName,
		FromEmail:    stored.FromEmail,
		Snippet:      stored.Snippet,
		ReceivedAt:   stored.ReceivedAt.UTC().Format(time.RFC3339),
		IsRead:       true,
	})
	return stored, nil
}

// buildOutgoingMessage 构造 go-mail 消息并序列化为原始 MIME 字节。
func buildOutgoingMessage(acc *store.Account, req SendRequest) (*gomail.Msg, []byte, string, error) {
	msg := gomail.NewMsg()
	if acc.DisplayName != "" {
		if err := msg.FromFormat(acc.DisplayName, acc.Email); err != nil {
			return nil, nil, "", fmt.Errorf("mail: 发件人地址不合法: %w", err)
		}
	} else if err := msg.From(acc.Email); err != nil {
		return nil, nil, "", fmt.Errorf("mail: 发件人地址不合法: %w", err)
	}
	if len(req.To) > 0 {
		if err := msg.To(req.To...); err != nil {
			return nil, nil, "", fmt.Errorf("mail: 收件人地址不合法: %w", err)
		}
	}
	if len(req.Cc) > 0 {
		if err := msg.Cc(req.Cc...); err != nil {
			return nil, nil, "", fmt.Errorf("mail: 抄送地址不合法: %w", err)
		}
	}
	if len(req.Bcc) > 0 {
		if err := msg.Bcc(req.Bcc...); err != nil {
			return nil, nil, "", fmt.Errorf("mail: 密送地址不合法: %w", err)
		}
	}
	msg.Subject(req.Subject)
	msg.SetDate()
	msg.SetMessageID()

	text := req.TextBody
	if strings.TrimSpace(text) == "" && req.HTMLBody != "" {
		text = htmlToText(req.HTMLBody)
	}
	if req.HTMLBody != "" {
		msg.SetBodyString(gomail.TypeTextHTML, req.HTMLBody)
		if text != "" {
			msg.AddAlternativeString(gomail.TypeTextPlain, text)
		}
	} else {
		msg.SetBodyString(gomail.TypeTextPlain, text)
	}

	// 回复关系头：ID 统一带尖括号。
	if req.InReplyTo != "" {
		msg.SetGenHeaderPreformatted(gomail.HeaderInReplyTo, "<"+strings.Trim(req.InReplyTo, "<> ")+">")
	}
	if len(req.References) > 0 {
		ids := make([]string, 0, len(req.References))
		for _, r := range req.References {
			r = strings.TrimSpace(r)
			if r == "" {
				continue
			}
			ids = append(ids, "<"+strings.Trim(r, "<> ")+">")
		}
		if len(ids) > 0 {
			msg.SetGenHeaderPreformatted(gomail.HeaderReferences, strings.Join(ids, " "))
		}
	}

	for _, att := range req.Attachments {
		name := strings.TrimSpace(att.Filename)
		if name == "" {
			name = "attachment.bin"
		}
		mimeType := strings.TrimSpace(att.MimeType)
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		opts := []gomail.FileOption{gomail.WithFileContentType(gomail.ContentType(mimeType))}
		if err := msg.AttachReader(name, bytes.NewReader(att.Data), opts...); err != nil {
			return nil, nil, "", fmt.Errorf("mail: 添加附件 %s 失败: %w", name, err)
		}
	}

	var buf bytes.Buffer
	if _, err := msg.WriteTo(&buf); err != nil {
		return nil, nil, "", fmt.Errorf("mail: 序列化邮件失败: %w", err)
	}
	messageID := strings.Trim(msg.GetMessageID(), "<> ")
	return msg, buf.Bytes(), messageID, nil
}

// newSMTPClient 按账号配置创建 SMTP 客户端（465 隐式 SSL / 其它端口 STARTTLS）。
func newSMTPClient(acc *store.Account, pass string, timeout time.Duration) (*gomail.Client, error) {
	port := acc.SMTPPort
	if port <= 0 {
		port = 465
	}
	opts := []gomail.Option{
		gomail.WithPort(port),
		gomail.WithTimeout(timeout),
		gomail.WithSMTPAuth(gomail.SMTPAuthPlain),
		gomail.WithUsername(acc.Username),
		gomail.WithPassword(pass),
		gomail.WithTLSConfig(tlsConfigFor(acc.SMTPHost)),
	}
	if acc.SMTPSecure {
		opts = append(opts, gomail.WithSSL())
	} else {
		opts = append(opts, gomail.WithTLSPolicy(gomail.TLSMandatory))
	}
	client, err := gomail.NewClient(acc.SMTPHost, opts...)
	if err != nil {
		return nil, fmt.Errorf("创建 SMTP 客户端失败: %w", err)
	}
	return client, nil
}

// deliverSMTP 用账号的 SMTP 配置投递邮件。
func (m *Manager) deliverSMTP(ctx context.Context, acc *store.Account, msg *gomail.Msg) error {
	pass, err := m.decryptPassword(acc)
	if err != nil {
		return err
	}
	client, err := newSMTPClient(acc, pass, smtpTimeout)
	if err != nil {
		return err
	}
	return client.DialAndSendWithContext(ctx, msg)
}

// enqueueSendRetry 写入失败重试任务（store.FinishJob 负责最多 3 次与退避）。
func (m *Manager) enqueueSendRetry(ctx context.Context, accountID string, req SendRequest, cause error) {
	payload, err := json.Marshal(req)
	if err != nil {
		m.log.Warn("序列化发信重试任务失败", "account", accountID, "error", err)
		return
	}
	job := &store.Job{
		Kind:      JobRetrySend,
		AccountID: accountID,
		Payload:   string(payload),
		RunAt:     time.Now().UTC().Add(time.Minute),
		LastError: cause.Error(),
	}
	if err := m.st.EnqueueJob(ctx, job); err != nil {
		m.log.Warn("写入发信重试任务失败", "account", accountID, "error", err)
		return
	}
	m.kickJobs()
}

// SendQueued 执行一个 retry_send 任务。
func (m *Manager) SendQueued(ctx context.Context, job *store.Job) error {
	if job == nil {
		return fmt.Errorf("mail: 任务为空")
	}
	var req SendRequest
	if job.Payload != "" {
		if err := json.Unmarshal([]byte(job.Payload), &req); err != nil {
			return fmt.Errorf("mail: 解析发信任务失败: %w", err)
		}
	}
	if req.AccountID == "" {
		req.AccountID = job.AccountID
	}
	if req.AccountID == "" {
		return fmt.Errorf("mail: 发信任务缺少账号 ID")
	}
	if _, err := m.send(ctx, req, false); err != nil {
		return err
	}
	return nil
}

// appendToSent 把已发送邮件 APPEND 到 Sent 文件夹，并尽量回读远端 UID。
// 返回 Sent 文件夹 ID（不存在时为空）与远端 UID（未取到为 0）。
func (m *Manager) appendToSent(ctx context.Context, acc *store.Account, raw []byte, messageID string) (string, uint32, error) {
	sent, err := m.st.GetFolderByRole(ctx, acc.ID, store.RoleSent)
	if err != nil {
		// 首次发信时文件夹可能尚未发现，补一次 LIST。
		if _, derr := m.DiscoverFolders(ctx, acc.ID); derr != nil {
			return "", 0, fmt.Errorf("mail: 查找已发送文件夹失败: %w", derr)
		}
		sent, err = m.st.GetFolderByRole(ctx, acc.ID, store.RoleSent)
		if err != nil {
			return "", 0, fmt.Errorf("mail: 未找到已发送文件夹: %w", err)
		}
	}

	conn, err := m.openIMAP(ctx, acc, imapDialTimeout)
	if err != nil {
		return sent.ID, 0, err
	}
	defer conn.close()

	if err := conn.client.Append(sent.RemotePath, []string{imap.SeenFlag}, time.Now(), newLiteral(raw)); err != nil {
		return sent.ID, 0, fmt.Errorf("mail: APPEND 到 %q 失败: %w", sent.RemotePath, err)
	}

	var uid uint32
	if _, serr := conn.client.Select(sent.RemotePath, true); serr == nil {
		criteria := &imap.SearchCriteria{
			Header: textproto.MIMEHeader{"Message-Id": []string{"<" + messageID + ">"}},
		}
		if uids, qerr := conn.client.UidSearch(criteria); qerr == nil {
			for _, u := range uids {
				if u > uid {
					uid = u
				}
			}
		}
	}
	return sent.ID, uid, nil
}

// insertSentMessage 在本地写入一条 is_draft=0 的已发送记录。
func (m *Manager) insertSentMessage(ctx context.Context, acc *store.Account, req SendRequest, raw []byte, messageID, folderID string, remoteUID uint32) *store.Message {
	if folderID == "" {
		return nil
	}
	now := time.Now().UTC()
	uid := remoteUID
	if uid == 0 {
		// APPEND 未回读到 UID 时，用本地最大值 +1 合成，满足唯一约束且不与远端冲突。
		uid = m.nextLocalUID(ctx, folderID)
	}

	attachments := make([]*store.AttachmentPayload, 0, len(req.Attachments))
	for _, att := range req.Attachments {
		mt := strings.TrimSpace(att.MimeType)
		if mt == "" {
			mt = "application/octet-stream"
		}
		attachments = append(attachments, &store.AttachmentPayload{
			Filename: strings.TrimSpace(att.Filename),
			MimeType: mt,
			Data:     att.Data,
		})
	}

	subject := req.Subject
	text := req.TextBody
	if strings.TrimSpace(text) == "" && req.HTMLBody != "" {
		text = htmlToText(req.HTMLBody)
	}
	msg := &store.Message{
		ID:             store.NewID(),
		AccountID:      acc.ID,
		FolderID:       folderID,
		RemoteUID:      uid,
		MessageID:      messageID,
		Subject:        subject,
		FromName:       acc.DisplayName,
		FromEmail:      acc.Email,
		To:             parseAddrs(req.To),
		Cc:             parseAddrs(req.Cc),
		Bcc:            parseAddrs(req.Bcc),
		SentAt:         &now,
		ReceivedAt:     now,
		Snippet:        makeSnippet(text),
		BodyText:       text,
		BodyHTML:       req.HTMLBody,
		HasAttachments: len(attachments) > 0,
		IsRead:         true,
		SizeBytes:      int64(len(raw)),
		CreatedAt:      now,
	}
	bundle := &store.MessageBundle{Message: msg, Attachments: attachments, Raw: raw}
	if _, err := m.st.InsertMessages(ctx, []*store.MessageBundle{bundle}); err != nil {
		m.log.Warn("写入已发送邮件记录失败", "account", acc.ID, "error", err)
		return msg
	}
	return msg
}

// nextLocalUID 返回文件夹内可用的合成 UID（本地最大值 +1）。
func (m *Manager) nextLocalUID(ctx context.Context, folderID string) uint32 {
	var maxUID int64
	if err := m.st.DB().QueryRowContext(ctx,
		"SELECT COALESCE(MAX(remote_uid), 0) FROM messages WHERE folder_id = ?", folderID).Scan(&maxUID); err != nil {
		return 0
	}
	if maxUID < 0 || maxUID >= int64(^uint32(0)) {
		return 0
	}
	return uint32(maxUID) + 1
}

// parseAddrs 把地址字符串解析为 store.Addr；解析失败时退化为纯邮箱。
func parseAddrs(list []string) []store.Addr {
	if len(list) == 0 {
		return nil
	}
	out := make([]store.Addr, 0, len(list))
	for _, raw := range list {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if a, err := mail.ParseAddress(raw); err == nil {
			out = append(out, store.Addr{Name: a.Name, Email: a.Address})
			continue
		}
		out = append(out, store.Addr{Email: raw})
	}
	return out
}
