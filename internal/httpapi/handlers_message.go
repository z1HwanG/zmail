package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"zmail/internal/realtime"
	"zmail/internal/store"
	"zmail/internal/verify"
)

// messageSummaryResponse 是列表接口的响应。
type messageSummaryResponse struct {
	Messages   []*store.MessageSummary `json:"messages"`
	NextCursor string                  `json:"nextCursor"`
}

// attachmentDTO 是附件元数据。
type attachmentDTO struct {
	ID        string `json:"id"`
	Filename  string `json:"filename"`
	MimeType  string `json:"mimeType"`
	Size      int64  `json:"size"`
	ContentID string `json:"contentId,omitempty"`
	URL       string `json:"url"`
}

// messageDetailDTO 是邮件详情，bodyHTML 已由服务端消毒。
type messageDetailDTO struct {
	ID             string          `json:"id"`
	AccountID      string          `json:"accountId"`
	AccountEmail   string          `json:"accountEmail"`
	AccountName    string          `json:"accountName"`
	FolderID       string          `json:"folderId"`
	FolderName     string          `json:"folderName"`
	ThreadID       string          `json:"threadId,omitempty"`
	RemoteUID      uint32          `json:"remoteUid"`
	MessageID      string          `json:"messageId,omitempty"`
	Subject        string          `json:"subject"`
	FromName       string          `json:"fromName"`
	FromEmail      string          `json:"fromEmail"`
	To             []store.Addr    `json:"to"`
	Cc             []store.Addr    `json:"cc"`
	Bcc            []store.Addr    `json:"bcc"`
	SentAt         *time.Time      `json:"sentAt,omitempty"`
	ReceivedAt     time.Time       `json:"receivedAt"`
	Snippet        string          `json:"snippet"`
	BodyText       string          `json:"bodyText"`
	BodyHTML       template.HTML   `json:"bodyHTML"`
	Code           string          `json:"code,omitempty"`
	HasAttachments bool            `json:"hasAttachments"`
	IsRead         bool            `json:"isRead"`
	IsStarred      bool            `json:"isStarred"`
	IsDeleted      bool            `json:"isDeleted"`
	IsDraft        bool            `json:"isDraft"`
	SizeBytes      int64           `json:"sizeBytes"`
	CreatedAt      time.Time       `json:"createdAt"`
	Attachments    []attachmentDTO `json:"attachments"`
}

func (s *Server) toMessageDetail(msg *store.Message, attachments []*store.Attachment) messageDetailDTO {
	atts := make([]attachmentDTO, 0, len(attachments))
	for _, a := range attachments {
		atts = append(atts, attachmentDTO{
			ID:        a.ID,
			Filename:  a.Filename,
			MimeType:  a.MimeType,
			Size:      a.Size,
			ContentID: a.ContentID,
			URL:       "/api/attachments/" + a.ID,
		})
	}
	bodyText := msg.BodyText
	if strings.TrimSpace(bodyText) == "" && msg.BodyHTML != "" {
		bodyText = htmlToText(msg.BodyHTML)
	}
	code := verify.Extract(msg.Subject, bodyText)
	return messageDetailDTO{
		ID:             msg.ID,
		AccountID:      msg.AccountID,
		AccountEmail:   msg.AccountEmail,
		AccountName:    msg.AccountName,
		FolderID:       msg.FolderID,
		FolderName:     msg.FolderName,
		ThreadID:       msg.ThreadID,
		RemoteUID:      msg.RemoteUID,
		MessageID:      msg.MessageID,
		Subject:        msg.Subject,
		FromName:       msg.FromName,
		FromEmail:      msg.FromEmail,
		To:             msg.To,
		Cc:             msg.Cc,
		Bcc:            msg.Bcc,
		SentAt:         msg.SentAt,
		ReceivedAt:     msg.ReceivedAt,
		Snippet:        msg.Snippet,
		BodyText:       bodyText,
		BodyHTML:       template.HTML(sanitizeHTML(msg.BodyHTML)),
		Code:           code,
		HasAttachments: msg.HasAttachments,
		IsRead:         msg.IsRead,
		IsStarred:      msg.IsStarred,
		IsDeleted:      msg.IsDeleted,
		IsDraft:        msg.IsDraft,
		SizeBytes:      msg.SizeBytes,
		CreatedAt:      msg.CreatedAt,
		Attachments:    atts,
	}
}

func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := queryInt(r, "limit", 50, 1, 200)
	filter := store.ListFilter{
		AccountID:   firstNonEmpty(q.Get("accountId"), q.Get("account")),
		FolderID:    firstNonEmpty(q.Get("folderId"), q.Get("folder")),
		Scope:       strings.ToLower(strings.TrimSpace(q.Get("scope"))),
		Cursor:      strings.TrimSpace(q.Get("cursor")),
		Limit:       limit,
		UnreadOnly:  queryBool(r, "unread"),
		StarredOnly: queryBool(r, "starred"),
	}
	if filter.FolderID == "" && filter.Scope == "" {
		filter.Scope = "inbox"
	}
	// before 兼容三种写法：store 游标 "时间|id"、毫秒时间戳、RFC3339 时间。
	if raw := strings.TrimSpace(firstNonEmpty(q.Get("before"), q.Get("cursor"))); raw != "" {
		if strings.Contains(raw, "|") {
			filter.Cursor = raw
		} else if ms, err := strconv.ParseInt(raw, 10, 64); err == nil {
			filter.Before = time.UnixMilli(ms).UTC()
		} else if t, err := time.Parse(time.RFC3339, raw); err == nil {
			filter.Before = t.UTC()
		} else {
			writeError(w, r, http.StatusBadRequest, codeBadRequest, "before 必须是分页游标、RFC3339 时间或毫秒时间戳")
			return
		}
	}
	ctx, cancel := contextWithTimeout(r, 15*time.Second)
	defer cancel()
	messages, next, err := s.st.ListMessages(ctx, filter)
	if err != nil {
		if errors.Is(err, store.ErrInvalid) {
			writeError(w, r, http.StatusBadRequest, codeBadRequest, err.Error())
			return
		}
		s.internalError(w, r, "查询邮件列表失败", err)
		return
	}
	if messages == nil {
		messages = []*store.MessageSummary{}
	}
	writeJSON(w, http.StatusOK, messageSummaryResponse{Messages: messages, NextCursor: next})
}

func (s *Server) handleGetMessage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx, cancel := contextWithTimeout(r, 15*time.Second)
	defer cancel()
	msg, atts, err := s.st.GetMessage(ctx, id)
	if err != nil {
		s.storeError(w, r, "邮件不存在", err)
		return
	}
	if msg.IsDeleted {
		writeError(w, r, http.StatusNotFound, codeNotFound, "邮件已删除")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"message": s.toMessageDetail(msg, atts)})
}

type patchMessageRequest struct {
	IsRead    *bool  `json:"isRead"`
	IsStarred *bool  `json:"isStarred"`
	AccountID string `json:"accountId"`
}

func (s *Server) handlePatchMessage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req patchMessageRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	if req.IsRead == nil && req.IsStarred == nil {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "至少需要提供 isRead 或 isStarred")
		return
	}
	ctx, cancel := contextWithTimeout(r, 15*time.Second)
	defer cancel()
	msg, _, err := s.st.GetMessage(ctx, id)
	if err != nil {
		s.storeError(w, r, "邮件不存在", err)
		return
	}
	if req.AccountID != "" && req.AccountID != msg.AccountID {
		writeError(w, r, http.StatusForbidden, codeForbidden, "邮件不属于该账号")
		return
	}
	updated, err := s.st.SetMessageFlags(ctx, id, req.IsRead, req.IsStarred)
	if err != nil {
		s.storeError(w, r, "更新邮件状态失败", err)
		return
	}
	unread, _ := s.st.UnreadTotal(ctx)
	s.hub.Publish(realtime.EventMailUpdate, mailUpdateEvent{
		ID:          updated.ID,
		AccountID:   updated.AccountID,
		FolderID:    updated.FolderID,
		IsRead:      req.IsRead,
		IsStarred:   req.IsStarred,
		UnreadTotal: unread,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"message": map[string]any{
			"id":        updated.ID,
			"accountId": updated.AccountID,
			"folderId":  updated.FolderID,
			"isRead":    updated.IsRead,
			"isStarred": updated.IsStarred,
		},
		"unreadTotal": unread,
	})
}

// mailUpdateEvent 是 mail:update SSE 事件的负载。
type mailUpdateEvent struct {
	ID          string `json:"id"`
	AccountID   string `json:"accountId"`
	FolderID    string `json:"folderId"`
	IsRead      *bool  `json:"isRead,omitempty"`
	IsStarred   *bool  `json:"isStarred,omitempty"`
	Deleted     bool   `json:"deleted,omitempty"`
	UnreadTotal int    `json:"unreadTotal"`
}

func (s *Server) handleDeleteMessage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx, cancel := contextWithTimeout(r, 15*time.Second)
	defer cancel()
	msg, _, err := s.st.GetMessage(ctx, id)
	if err != nil {
		s.storeError(w, r, "邮件不存在", err)
		return
	}
	accountID := firstNonEmpty(r.URL.Query().Get("accountId"), r.URL.Query().Get("account"))
	if accountID != "" && accountID != msg.AccountID {
		writeError(w, r, http.StatusForbidden, codeForbidden, "邮件不属于该账号")
		return
	}
	if err := s.st.SoftDeleteMessage(ctx, id); err != nil {
		s.storeError(w, r, "删除邮件失败", err)
		return
	}
	// 异步回写 IMAP：入队 delete_remote 任务，由邮件引擎在后台执行；
	// 入队失败时写 sync_log 以便排查。
	s.enqueueRemoteDelete(msg)
	unread, _ := s.st.UnreadTotal(ctx)
	s.hub.Publish(realtime.EventMailUpdate, mailUpdateEvent{
		ID:          msg.ID,
		AccountID:   msg.AccountID,
		FolderID:    msg.FolderID,
		Deleted:     true,
		UnreadTotal: unread,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) enqueueRemoteDelete(msg *store.Message) {
	payload, err := json.Marshal(map[string]any{
		"messageId": msg.ID,
		"accountId": msg.AccountID,
		"folderId":  msg.FolderID,
		"remoteUid": msg.RemoteUID,
		"action":    "delete",
	})
	if err != nil {
		s.log.Warn("构造远程删除任务失败", "message_id", msg.ID, "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	now := time.Now().UTC()
	job := &store.Job{
		ID:        store.NewID(),
		Kind:      "delete_remote",
		AccountID: msg.AccountID,
		Payload:   string(payload),
		RunAt:     now,
		Status:    store.JobPending,
		CreatedAt: now,
	}
	if err := s.st.EnqueueJob(ctx, job); err != nil {
		s.log.Warn("远程删除任务入队失败", "message_id", msg.ID, "err", err)
		_ = s.st.AddSyncLog(context.Background(), msg.AccountID, "error", "远程删除任务入队失败: "+err.Error())
	}
}

func (s *Server) handleListFolders(w http.ResponseWriter, r *http.Request) {
	accountID := firstNonEmpty(r.URL.Query().Get("accountId"), r.URL.Query().Get("account"))
	ctx, cancel := contextWithTimeout(r, 10*time.Second)
	defer cancel()
	folders, err := s.st.ListFolders(ctx, accountID)
	if err != nil {
		s.internalError(w, r, "查询文件夹失败", err)
		return
	}
	// 用 messages 表实时统计覆盖 folders.unread_count，避免同步后计数滞后。
	unread, err := s.st.UnreadCounts(ctx)
	if err != nil {
		s.log.Warn("统计未读数失败", "err", err)
		unread = map[string]int{}
	}
	out := make([]map[string]any, 0, len(folders))
	for _, f := range folders {
		unreadCount := f.UnreadCount
		if n, ok := unread[f.ID]; ok {
			unreadCount = n
		}
		out = append(out, map[string]any{
			"id":           f.ID,
			"accountId":    f.AccountID,
			"remotePath":   f.RemotePath,
			"name":         f.Name,
			"role":         f.Role,
			"uidValidity":  f.UIDValidity,
			"lastUid":      f.LastUID,
			"unreadCount":  unreadCount,
			"totalCount":   f.TotalCount,
			"enabled":      f.Enabled,
			"lastSyncedAt": f.LastSyncedAt,
			"backfillUID":  f.BackfillUID,
			"backfillDone": f.BackfillUID < 0,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"folders": out})
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	accountID := firstNonEmpty(r.URL.Query().Get("accountId"), r.URL.Query().Get("account"))
	limit := queryInt(r, "limit", 50, 1, 100)
	if query == "" {
		writeJSON(w, http.StatusOK, messageSummaryResponse{Messages: []*store.MessageSummary{}, NextCursor: ""})
		return
	}
	ctx, cancel := contextWithTimeout(r, 20*time.Second)
	defer cancel()
	messages, err := s.st.Search(ctx, query, accountID, limit)
	if err != nil {
		if errors.Is(err, store.ErrInvalid) {
			writeError(w, r, http.StatusBadRequest, codeBadRequest, err.Error())
			return
		}
		s.internalError(w, r, "搜索邮件失败", err)
		return
	}
	if messages == nil {
		messages = []*store.MessageSummary{}
	}
	writeJSON(w, http.StatusOK, messageSummaryResponse{Messages: messages, NextCursor: ""})
}

func (s *Server) handleGetAttachment(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx, cancel := contextWithTimeout(r, 15*time.Second)
	defer cancel()

	var (
		filename, mimeType, contentID, sha, storageKey string
		size                                           int64
	)
	err := s.st.DB().QueryRowContext(ctx,
		"SELECT filename, mime_type, size, COALESCE(content_id,''), sha256, storage_key FROM attachments WHERE id = ?", id).
		Scan(&filename, &mimeType, &size, &contentID, &sha, &storageKey)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, r, http.StatusNotFound, codeNotFound, "附件不存在")
			return
		}
		s.internalError(w, r, "查询附件失败", err)
		return
	}
	key := strings.TrimSpace(storageKey)
	if key == "" {
		key = strings.TrimSpace(sha)
	}
	f, _, err := s.st.Blobs().OpenAttachment(key)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, r, http.StatusNotFound, codeNotFound, "附件内容不存在")
			return
		}
		s.internalError(w, r, "打开附件失败", err)
		return
	}
	defer f.Close()

	safeName := sanitizeFilename(filename)
	ctype := strings.TrimSpace(mimeType)
	if ctype == "" {
		if ext := extOf(safeName); ext != "" {
			ctype = mime.TypeByExtension(ext)
		}
	}
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("Content-Disposition", contentDisposition(safeName))
	h.Set("X-Content-Type-Options", "nosniff")
	if sha != "" {
		h.Set("ETag", "\""+sha+"\"")
	}
	h.Set("Cache-Control", "private, max-age=300")
	http.ServeContent(w, r, safeName, time.Time{}, f)
}

func extOf(name string) string {
	idx := strings.LastIndex(name, ".")
	if idx < 0 || idx == len(name)-1 {
		return ""
	}
	return name[idx:]
}

// sanitizeFilename 去掉路径与控制字符，避免响应头注入。
func sanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		switch {
		case r < 32, r == 127, r == '"', r == '/', r == '\\':
			return '_'
		}
		return r
	}, name)
	name = strings.TrimSpace(strings.Trim(name, "."))
	if name == "" || name == ".." {
		return "attachment"
	}
	name = truncateUTF8(name, 180)
	return name
}

func contentDisposition(name string) string {
	ascii := make([]rune, 0, len(name))
	for _, r := range name {
		if r < 128 && r >= 32 && r != '"' && r != '\\' {
			ascii = append(ascii, r)
		} else {
			ascii = append(ascii, '_')
		}
	}
	fallback := string(ascii)
	if strings.TrimSpace(fallback) == "" {
		fallback = "attachment"
	}
	return fmt.Sprintf("attachment; filename=\"%s\"; filename*=UTF-8''%s", fallback, url.PathEscape(name))
}
