package mail

import (
	"bytes"
	"fmt"
	"html"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset" // 注册 GBK/GB18030/ISO-8859-1 等字符集解码器
	gomail "github.com/emersion/go-message/mail"

	"zmail/internal/store"
)

// 解析阶段的资源上限，避免恶意/异常邮件耗尽内存。
const (
	maxPartBytes    = 32 << 20 // 单个 MIME 部分最大 32MiB
	snippetMaxRunes = 200      // 列表摘要最大字符数（与 store 保持一致）
)

// ParsedMessage 是一封邮件解析后的结果。
type ParsedMessage struct {
	MessageID   string
	InReplyTo   string
	References  []string
	Subject     string
	FromName    string
	FromEmail   string
	To          []store.Addr
	Cc          []store.Addr
	Bcc         []store.Addr
	Date        time.Time
	TextBody    string
	HTMLBody    string
	Snippet     string
	Attachments []*store.AttachmentPayload
	Raw         []byte
	Size        int64

	// dateMissing 表示邮件头里没有可解析的 Date，Date 已回退为当前时间。
	dateMissing bool
}

// Parse 从 r 读取并按 MIME 结构解析一封邮件。
func Parse(r io.Reader) (*ParsedMessage, error) {
	if r == nil {
		return nil, fmt.Errorf("mail: 邮件内容为空")
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("mail: 读取邮件内容失败: %w", err)
	}
	return ParseBytes(data)
}

// ParseBytes 解析原始邮件字节。
//
// 返回的 ParsedMessage 中正文已完成传输编码与字符集解码（UTF-8），
// HTML 正文只做提取不做消毒（渲染前的消毒由 httpapi 负责）。
func ParseBytes(data []byte) (*ParsedMessage, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("mail: 邮件内容为空")
	}

	e, err := message.Read(bytes.NewReader(data))
	if e == nil {
		return nil, fmt.Errorf("mail: 解析邮件头失败: %w", err)
	}
	// 未知字符集/传输编码时 go-message 仍返回可读实体，降级为原始字节继续解析。
	if err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err) {
		return nil, fmt.Errorf("mail: 解析邮件失败: %w", err)
	}

	pm := &ParsedMessage{
		Raw:  data,
		Size: int64(len(data)),
	}
	h := gomail.Header{Header: e.Header}

	// 头部字段：RFC2047 由 go-message 的 charset 解码器处理。
	if subject, serr := h.Subject(); serr == nil {
		pm.Subject = strings.TrimSpace(subject)
	} else {
		pm.Subject = strings.TrimSpace(h.Get("Subject"))
	}
	if date, derr := h.Date(); derr == nil && !date.IsZero() {
		pm.Date = date
	} else {
		pm.Date = time.Now().UTC()
		pm.dateMissing = true
	}
	if id, merr := h.MessageID(); merr == nil {
		pm.MessageID = strings.TrimSpace(id)
	} else {
		pm.MessageID = strings.Trim(strings.TrimSpace(h.Get("Message-Id")), "<> ")
	}
	if refs, rerr := h.MsgIDList("References"); rerr == nil {
		pm.References = refs
	}
	if irt, ierr := h.MsgIDList("In-Reply-To"); ierr == nil && len(irt) > 0 {
		pm.InReplyTo = irt[0]
	}
	if from, ferr := h.AddressList("From"); ferr == nil && len(from) > 0 {
		pm.FromName = strings.TrimSpace(from[0].Name)
		pm.FromEmail = strings.TrimSpace(from[0].Address)
	}
	if to, terr := h.AddressList("To"); terr == nil {
		pm.To = toStoreAddrs(to)
	}
	if cc, cerr := h.AddressList("Cc"); cerr == nil {
		pm.Cc = toStoreAddrs(cc)
	}
	if bcc, berr := h.AddressList("Bcc"); berr == nil {
		pm.Bcc = toStoreAddrs(bcc)
	}

	// 展开 MIME 树，区分正文与附件。
	var inlineSeq int
	walkErr := e.Walk(func(_ []int, ent *message.Entity, partErr error) error {
		if ent == nil {
			return nil
		}
		mt, _, _ := ent.Header.ContentType()
		mt = strings.ToLower(strings.TrimSpace(mt))
		if strings.HasPrefix(mt, "multipart/") {
			return nil
		}

		disp, _, _ := ent.Header.ContentDisposition()
		filename := partFilename(&ent.Header)
		contentID := strings.Trim(strings.TrimSpace(ent.Header.Get("Content-ID")), "<>")
		isAttachment := strings.EqualFold(disp, "attachment") || filename != ""
		if !isAttachment {
			switch mt {
			case "text/plain", "text/html", "":
				// 正文
			default:
				// 无文件名的非文本部分（内嵌图片等）也按附件保存，便于前端按 Content-ID 引用。
				isAttachment = true
			}
		}

		body, rerr := io.ReadAll(io.LimitReader(ent.Body, maxPartBytes))
		if rerr != nil && !message.IsUnknownCharset(rerr) && !message.IsUnknownEncoding(rerr) {
			// 单个部分读取失败不影响其它部分。
			return nil
		}
		_ = partErr

		if !isAttachment {
			text := normalizeNewlines(string(body))
			if mt == "text/html" {
				if pm.HTMLBody == "" && strings.TrimSpace(text) != "" {
					pm.HTMLBody = text
				}
			} else if pm.TextBody == "" && strings.TrimSpace(text) != "" {
				pm.TextBody = text
			}
			return nil
		}

		if filename == "" {
			inlineSeq++
			filename = "inline-" + strconv.Itoa(inlineSeq) + extForMIME(mt)
		}
		if mt == "" {
			mt = "application/octet-stream"
		}
		pm.Attachments = append(pm.Attachments, &store.AttachmentPayload{
			Filename:  filename,
			MimeType:  mt,
			ContentID: contentID,
			Data:      body,
		})
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("mail: 遍历 MIME 部分失败: %w", walkErr)
	}

	pm.TextBody = strings.TrimSpace(pm.TextBody)
	pm.HTMLBody = strings.TrimSpace(pm.HTMLBody)
	if pm.TextBody == "" && pm.HTMLBody != "" {
		// 只有 HTML 正文时生成纯文本，供全文检索与验证码识别使用。
		pm.TextBody = strings.TrimSpace(htmlToText(pm.HTMLBody))
	}
	pm.Snippet = makeSnippet(pm.TextBody)
	return pm, nil
}

func toStoreAddrs(list []*gomail.Address) []store.Addr {
	if len(list) == 0 {
		return nil
	}
	out := make([]store.Addr, 0, len(list))
	for _, a := range list {
		if a == nil {
			continue
		}
		out = append(out, store.Addr{
			Name:  strings.TrimSpace(a.Name),
			Email: strings.TrimSpace(a.Address),
		})
	}
	return out
}

// partFilename 依次从 Content-Disposition 的 filename 与 Content-Type 的 name 取文件名。
func partFilename(h *message.Header) string {
	if _, params, err := h.ContentDisposition(); err == nil {
		if v := strings.TrimSpace(params["filename"]); v != "" {
			return v
		}
	}
	if _, params, err := h.ContentType(); err == nil {
		if v := strings.TrimSpace(params["name"]); v != "" {
			return v
		}
	}
	return ""
}

var mimeExts = map[string]string{
	"image/jpeg":      ".jpg",
	"image/png":       ".png",
	"image/gif":       ".gif",
	"image/webp":      ".webp",
	"image/svg+xml":   ".svg",
	"image/bmp":       ".bmp",
	"application/pdf": ".pdf",
}

func extForMIME(mt string) string {
	if ext, ok := mimeExts[mt]; ok {
		return ext
	}
	return ""
}

var (
	reScript   = regexp.MustCompile("(?is)<(?:script|style)\\b[^>]*>.*?</(?:script|style)\\s*>")
	reComment  = regexp.MustCompile("(?s)<!--.*?-->")
	reBreakTag = regexp.MustCompile("(?i)<\\s*(?:br|/p|/div|/li|/tr|/h[1-6]|/blockquote|/pre)\\b[^>]*>")
	reAnyTag   = regexp.MustCompile("(?s)<[^>]*>")
	reHSpace   = regexp.MustCompile("[ \\t\\f\\v\\x{00a0}]+")
	reBlank    = regexp.MustCompile("\\n{3,}")
)

// htmlToText 把 HTML 粗略转成纯文本：去脚本/样式/标签，还原实体，压缩空白。
func htmlToText(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	s = reScript.ReplaceAllString(s, " ")
	s = reComment.ReplaceAllString(s, " ")
	s = reBreakTag.ReplaceAllString(s, "\n")
	s = reAnyTag.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	s = normalizeNewlines(s)
	s = reHSpace.ReplaceAllString(s, " ")
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSpace(line)
	}
	s = strings.Join(lines, "\n")
	s = reBlank.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// normalizeNewlines 统一换行并去掉 CR。
func normalizeNewlines(s string) string {
	return strings.ReplaceAll(s, "\r\n", "\n")
}

// makeSnippet 把正文压成单行摘要并截断到 200 个字符。
func makeSnippet(s string) string {
	s = htmlToText(s)
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	s = strings.Join(fields, " ")
	if utf8.RuneCountInString(s) <= snippetMaxRunes {
		return s
	}
	runes := []rune(s)
	return string(runes[:snippetMaxRunes])
}
