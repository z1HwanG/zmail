package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	netmail "net/mail"
	"strings"
	"time"

	"zmail/internal/mail"
)

const (
	maxSendBody        = 40 << 20
	maxAttachmentBytes = 10 << 20
	maxAttachments     = 20
	maxAttachmentTotal = 25 << 20
)

// stringList 兼容 JSON 中 "a@b.com" 与 ["a@b.com","c@d.com"] 两种写法。
type stringList []string

func (s *stringList) UnmarshalJSON(data []byte) error {
	var single string
	if err := json.Unmarshal(data, &single); err == nil {
		*s = splitRecipients(single)
		return nil
	}
	var arr []string
	if err := json.Unmarshal(data, &arr); err == nil {
		out := make([]string, 0, len(arr))
		for _, item := range arr {
			out = append(out, splitRecipients(item)...)
		}
		*s = out
		return nil
	}
	return errors.New("收件人必须是字符串或字符串数组")
}

func splitRecipients(raw string) []string {
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\r'
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

type sendAttachmentReq struct {
	Filename      string `json:"filename"`
	MimeType      string `json:"mimeType"`
	ContentType   string `json:"contentType"`
	Data          string `json:"data"`
	ContentBase64 string `json:"contentBase64"`
}

type sendRequest struct {
	AccountID   string              `json:"accountId"`
	To          stringList          `json:"to"`
	Cc          stringList          `json:"cc"`
	Bcc         stringList          `json:"bcc"`
	Subject     string              `json:"subject"`
	Text        string              `json:"text"`
	TextBody    string              `json:"textBody"`
	HTML        string              `json:"html"`
	HTMLBody    string              `json:"htmlBody"`
	InReplyTo   string              `json:"inReplyTo"`
	References  stringList          `json:"references"`
	Attachments []sendAttachmentReq `json:"attachments"`
}

func (s *Server) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	if s.mail == nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "邮件引擎未就绪")
		return
	}
	req, err := s.parseSendRequest(w, r)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}

	to, err := normalizeAddresses(req.To)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "收件人格式错误: "+err.Error())
		return
	}
	cc, err := normalizeAddresses(req.Cc)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "抄送格式错误: "+err.Error())
		return
	}
	bcc, err := normalizeAddresses(req.Bcc)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "密送格式错误: "+err.Error())
		return
	}
	if len(to) == 0 {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "至少需要一个收件人")
		return
	}
	subject := strings.TrimSpace(req.Subject)
	if subject == "" {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "主题不能为空")
		return
	}
	if len(subject) > 998 {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "主题过长")
		return
	}
	textBody := strings.TrimSpace(req.Text)
	if textBody == "" {
		textBody = strings.TrimSpace(req.TextBody)
	}
	htmlBody := strings.TrimSpace(req.HTML)
	if htmlBody == "" {
		htmlBody = strings.TrimSpace(req.HTMLBody)
	}
	if textBody == "" && htmlBody == "" {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "正文不能为空")
		return
	}

	ctx, cancel := contextWithTimeout(r, 10*time.Second)
	defer cancel()
	if _, err := s.st.GetAccount(ctx, req.AccountID); err != nil {
		s.storeError(w, r, "账号不存在", err)
		return
	}

	atts, err := s.decodeSendAttachments(req.Attachments)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	sendReq := mail.SendRequest{
		AccountID:   req.AccountID,
		To:          to,
		Cc:          cc,
		Bcc:         bcc,
		Subject:     subject,
		TextBody:    textBody,
		HTMLBody:    htmlBody,
		InReplyTo:   strings.TrimSpace(req.InReplyTo),
		References:  req.References,
		Attachments: atts,
	}

	// 发信不允许被客户端断开打断，使用 WithoutCancel + 120s 超时。
	sendCtx, sendCancel := context.WithTimeout(context.WithoutCancel(r.Context()), 120*time.Second)
	defer sendCancel()
	msg, err := s.mail.Send(sendCtx, sendReq)
	if err != nil {
		// mail.Manager.Send 在 SMTP 失败时已自行写入 retry_send 任务，这里不重复入队。
		s.log.Warn("发送邮件失败", "account_id", req.AccountID, "err", err)
		writeError(w, r, http.StatusBadGateway, codeUpstreamError, "发送失败，已加入重试队列: "+err.Error())
		return
	}
	resp := map[string]any{"ok": true}
	if msg != nil {
		resp["messageId"] = msg.ID
		resp["accountId"] = msg.AccountID
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) parseSendRequest(w http.ResponseWriter, r *http.Request) (*sendRequest, error) {
	ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	if strings.HasPrefix(ct, "multipart/form-data") {
		return s.parseMultipartSend(w, r)
	}
	var req sendRequest
	// 发信 JSON 可能内联 base64 附件，使用更大的请求体上限。
	if err := decodeJSONBodyLimit(w, r, &req, maxSendBody); err != nil {
		return nil, fmt.Errorf("请求体格式错误: %w", err)
	}
	return &req, nil
}

func (s *Server) parseMultipartSend(w http.ResponseWriter, r *http.Request) (*sendRequest, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxSendBody)
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		return nil, fmt.Errorf("解析 multipart 请求失败: %w", err)
	}
	form := r.MultipartForm
	field := func(name string) string {
		if form == nil {
			return ""
		}
		if vals := form.Value[name]; len(vals) > 0 {
			return vals[0]
		}
		return ""
	}
	req := &sendRequest{
		AccountID:  field("accountId"),
		To:         splitRecipients(strings.Join(form.Value["to"], ",")),
		Cc:         splitRecipients(strings.Join(form.Value["cc"], ",")),
		Bcc:        splitRecipients(strings.Join(form.Value["bcc"], ",")),
		Subject:    field("subject"),
		Text:       field("text"),
		TextBody:   field("textBody"),
		HTML:       field("html"),
		HTMLBody:   field("htmlBody"),
		InReplyTo:  field("inReplyTo"),
		References: splitRecipients(strings.Join(form.Value["references"], ",")),
	}
	if req.AccountID == "" {
		return nil, errors.New("缺少 accountId")
	}

	var files []*multipart.FileHeader
	for _, key := range []string{"attachments", "files", "file"} {
		files = append(files, form.File[key]...)
	}
	var total int64
	for _, fh := range files {
		if fh.Size > maxAttachmentBytes {
			return nil, fmt.Errorf("附件 %s 超过 10MB 限制", fh.Filename)
		}
		file, err := fh.Open()
		if err != nil {
			return nil, fmt.Errorf("读取附件 %s 失败: %w", fh.Filename, err)
		}
		data, err := io.ReadAll(io.LimitReader(file, maxAttachmentBytes+1))
		file.Close()
		if err != nil {
			return nil, fmt.Errorf("读取附件 %s 失败: %w", fh.Filename, err)
		}
		if int64(len(data)) > maxAttachmentBytes {
			return nil, fmt.Errorf("附件 %s 超过 10MB 限制", fh.Filename)
		}
		total += int64(len(data))
		if total > maxAttachmentTotal {
			return nil, errors.New("附件总大小超过 25MB 限制")
		}
		req.Attachments = append(req.Attachments, sendAttachmentReq{
			Filename: fh.Filename,
			MimeType: fh.Header.Get("Content-Type"),
			Data:     base64.StdEncoding.EncodeToString(data),
		})
	}
	if len(req.Attachments) > maxAttachments {
		return nil, fmt.Errorf("附件数量超过 %d 个限制", maxAttachments)
	}
	return req, nil
}

func (s *Server) decodeSendAttachments(items []sendAttachmentReq) ([]mail.Attachment, error) {
	if len(items) > maxAttachments {
		return nil, fmt.Errorf("附件数量超过 %d 个限制", maxAttachments)
	}
	out := make([]mail.Attachment, 0, len(items))
	var total int64
	for _, item := range items {
		raw := strings.TrimSpace(item.Data)
		if raw == "" {
			raw = strings.TrimSpace(item.ContentBase64)
		}
		if raw == "" {
			continue
		}
		if idx := strings.Index(raw, ","); idx >= 0 && strings.Contains(raw[:idx], "base64") {
			raw = raw[idx+1:]
		}
		data, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			data, err = base64.RawStdEncoding.DecodeString(raw)
		}
		if err != nil {
			data, err = base64.RawURLEncoding.DecodeString(raw)
		}
		if err != nil {
			return nil, fmt.Errorf("附件 %s 的 base64 内容不合法", item.Filename)
		}
		if int64(len(data)) > maxAttachmentBytes {
			return nil, fmt.Errorf("附件 %s 超过 10MB 限制", item.Filename)
		}
		total += int64(len(data))
		if total > maxAttachmentTotal {
			return nil, errors.New("附件总大小超过 25MB 限制")
		}
		mimeType := strings.TrimSpace(item.MimeType)
		if mimeType == "" {
			mimeType = strings.TrimSpace(item.ContentType)
		}
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		out = append(out, mail.Attachment{
			Filename: sanitizeFilename(item.Filename),
			MimeType: mimeType,
			Data:     data,
		})
	}
	return out, nil
}

// normalizeAddresses 把地址列表规范化并去重。
func normalizeAddresses(list []string) ([]string, error) {
	out := make([]string, 0, len(list))
	seen := make(map[string]bool, len(list))
	for _, raw := range list {
		for _, part := range splitRecipients(raw) {
			addr, err := netmail.ParseAddress(part)
			if err != nil {
				return nil, fmt.Errorf("%q 不是合法邮箱地址", part)
			}
			key := strings.ToLower(addr.Address)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, addr.Address)
		}
	}
	return out, nil
}
