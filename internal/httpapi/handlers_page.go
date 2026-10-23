package httpapi

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"zmail/internal/mail"
	"zmail/internal/store"
	"zmail/internal/web"
)

//go:embed templates/*.html
var templatesFS embed.FS

// pageData 是所有页面模板的数据结构。
type pageData struct {
	Title        string
	Section      string
	Nonce        string
	AssetVersion string
	ZMail        template.JS
	CSRF         string
	BaseURL      string
	PublicURL    string
	Error        string
	Next         string
	Status       int

	AccountID string
	FolderID  string
	MessageID string
	Scope     string

	Accounts  []*store.Account
	Sidebar   []sidebarAccount
	Messages  []*store.MessageSummary
	Current   *messageDetailDTO
	Stats     store.Stats
	Providers []mail.Provider
}

type sidebarAccount struct {
	Account *store.Account
	Color   string
	Unread  int
	Folders []sidebarFolder
}

type sidebarFolder struct {
	Folder *store.Folder
	Unread int
	Active bool
}

func parseTemplates() (map[string]*template.Template, error) {
	pages := []string{"login.html", "mail.html", "settings_accounts.html", "settings_general.html"}
	out := make(map[string]*template.Template, len(pages))
	for _, page := range pages {
		t := template.New(page).Funcs(templateFuncMap())
		parsed, err := t.ParseFS(templatesFS, "templates/partials.html", "templates/"+page)
		if err != nil {
			return nil, fmt.Errorf("httpapi: 解析模板 %s 失败: %w", page, err)
		}
		out[page] = parsed
	}
	return out, nil
}

func templateFuncMap() template.FuncMap {
	return template.FuncMap{
		"formatTime":   formatTimeValue,
		"formatDate":   formatTimeValue,
		"accountColor": accountColor,
		"fromLabel": func(name, email string) string {
			if strings.TrimSpace(name) != "" {
				return name
			}
			return email
		},
		"folderClass": func(role string) string { return "role-" + role },
		"folderIcon":  folderIconSVG,
	}
}

// formatTimeValue 同时接受 time.Time 与 *time.Time（模板中可空的时间字段）。
func formatTimeValue(v any) string {
	var t time.Time
	switch value := v.(type) {
	case time.Time:
		t = value
	case *time.Time:
		if value == nil {
			return ""
		}
		t = *value
	default:
		return ""
	}
	if t.IsZero() {
		return ""
	}
	local := t.Local()
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, local.Location())
	switch {
	case day.Equal(today):
		return local.Format("15:04")
	case day.Equal(today.AddDate(0, 0, -1)):
		return "昨天"
	case local.Year() == now.Year():
		return local.Format("01-02")
	default:
		return local.Format("2006-01-02")
	}
}

// accountColor 根据账号 ID 稳定地生成一个颜色，供前端色点使用。
func accountColor(id string) string {
	palette := []string{"#1c4f8a", "#cf4436", "#3f7258", "#8a5a44", "#5d6f87", "#7a5c74", "#4e7a7c", "#9a6a22"}
	if id == "" {
		return "#888888"
	}
	var sum uint32
	for i := 0; i < len(id); i++ {
		sum = sum*31 + uint32(id[i])
	}
	return palette[int(sum%uint32(len(palette)))]
}

// folderIconSVG 返回侧栏文件夹的线性图标，避免使用 emoji 或单个汉字。
var folderIconSVGs = map[string]string{
	"all":     `<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 6h16v13H4z"/><path d="M4 10h16"/><path d="M9 14h6"/></svg>`,
	"inbox":   `<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 5h16v14H4z"/><path d="M4 13h4l2 2h4l2-2h4"/></svg>`,
	"sent":    `<svg viewBox="0 0 24 24" aria-hidden="true"><path d="m3 11 18-8-7 18-3-7-8-3Z"/><path d="M14 10 8 16"/></svg>`,
	"drafts":  `<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 20h4L19 9l-4-4L4 16v4Z"/><path d="m13 7 4 4"/></svg>`,
	"trash":   `<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 7h16"/><path d="M9 7V4h6v3"/><path d="m6 7 1 13h10l1-13"/></svg>`,
	"spam":    `<svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12" r="8"/><path d="m8 8 8 8"/></svg>`,
	"archive": `<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 8h16v12H4z"/><path d="M3 4h18v4H3z"/><path d="M9 12h6"/></svg>`,
	"custom":  `<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M3 6h7l2 2h9v11H3z"/></svg>`,
}

func folderIconSVG(role string) template.HTML {
	if s, ok := folderIconSVGs[role]; ok {
		return template.HTML(s)
	}
	return template.HTML(folderIconSVGs["custom"])
}

// renderPage 渲染页面：生成 CSP nonce、注入 window.__ZMAIL__。
func (s *Server) renderPage(w http.ResponseWriter, r *http.Request, name string, data pageData) {
	tpl, ok := s.templates[name]
	if !ok {
		s.log.Error("页面模板不存在", "template", name)
		http.Error(w, "页面模板不存在", http.StatusInternalServerError)
		return
	}
	nonce, err := randomToken(16)
	if err != nil {
		s.log.Error("生成 CSP nonce 失败", "err", err)
		http.Error(w, "服务器内部错误", http.StatusInternalServerError)
		return
	}
	data.Nonce = nonce
	data.AssetVersion = web.Version()
	data.PublicURL = s.cfg.PublicURL
	data.BaseURL = s.cfg.PublicURL
	if data.Title == "" {
		data.Title = "zMail · 聚合邮箱"
	}
	if sess := sessionFromContext(r.Context()); sess != nil {
		data.CSRF = sess.CSRF
	}
	zm, err := json.Marshal(map[string]string{
		"accountId": data.AccountID,
		"folderId":  data.FolderID,
		"messageId": data.MessageID,
		"csrfToken": data.CSRF,
		"baseUrl":   data.BaseURL,
	})
	if err != nil {
		s.log.Error("序列化 window.__ZMAIL__ 失败", "err", err)
		http.Error(w, "服务器内部错误", http.StatusInternalServerError)
		return
	}
	data.ZMail = template.JS(zm)

	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, name, data); err != nil {
		s.log.Error("渲染页面失败", "template", name, "err", err)
		http.Error(w, "页面渲染失败", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", pageCSP(nonce))
	status := data.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

func (s *Server) handleMailPage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	accountID := firstNonEmpty(q.Get("account"), q.Get("accountId"))
	folderID := firstNonEmpty(q.Get("folder"), q.Get("folderId"))
	messageID := firstNonEmpty(q.Get("message"), q.Get("messageId"), q.Get("m"))
	scope := strings.ToLower(strings.TrimSpace(q.Get("scope")))

	ctx, cancel := contextWithTimeout(r, 20*time.Second)
	defer cancel()

	data := pageData{
		Title:     "zMail · 聚合邮箱",
		Section:   "mail",
		AccountID: accountID,
		FolderID:  folderID,
		MessageID: messageID,
		Scope:     scope,
	}

	accounts, err := s.st.ListAccounts(ctx)
	if err != nil {
		s.log.Warn("页面加载账号失败", "err", err)
		s.internalError(w, r, "加载账号失败", err)
		return
	}
	folders, err := s.st.ListFolders(ctx, "")
	if err != nil {
		s.log.Warn("页面加载文件夹失败", "err", err)
		folders = nil
	}
	unread, err := s.st.UnreadCounts(ctx)
	if err != nil {
		s.log.Warn("页面加载未读数失败", "err", err)
		unread = map[string]int{}
	}
	// folderID 存在时补全 accountID 并校验归属。
	if folderID != "" {
		for _, f := range folders {
			if f.ID == folderID {
				if accountID == "" {
					accountID = f.AccountID
					data.AccountID = accountID
				} else if f.AccountID != accountID {
					writeError(w, r, http.StatusForbidden, codeForbidden, "文件夹不属于该账号")
					return
				}
				break
			}
		}
	}
	data.Accounts = accounts
	data.Sidebar = buildSidebar(accounts, folders, unread, accountID, folderID)

	filter := store.ListFilter{AccountID: accountID, FolderID: folderID, Limit: 50}
	if folderID == "" {
		filter.Scope = "inbox"
	}
	if scope != "" && folderID == "" {
		filter.Scope = scope
	}
	messages, _, err := s.st.ListMessages(ctx, filter)
	if err != nil {
		s.log.Warn("页面加载邮件列表失败", "err", err)
		messages = nil
	}
	if messages == nil {
		messages = []*store.MessageSummary{}
	}
	data.Messages = messages

	if messageID != "" {
		msg, atts, err := s.st.GetMessage(ctx, messageID)
		if err != nil {
			if !isNotFound(err) {
				s.log.Warn("页面加载邮件详情失败", "err", err)
			}
			data.Error = "邮件不存在或已被删除"
		} else if msg.IsDeleted {
			data.Error = "邮件已删除"
		} else {
			detail := s.toMessageDetail(msg, atts)
			data.Current = &detail
			data.AccountID = msg.AccountID
			data.FolderID = msg.FolderID
			data.MessageID = msg.ID
		}
	}
	s.renderPage(w, r, "mail.html", data)
}

func buildSidebar(accounts []*store.Account, folders []*store.Folder, unread map[string]int, activeAccount, activeFolder string) []sidebarAccount {
	out := make([]sidebarAccount, 0, len(accounts))
	for _, acc := range accounts {
		item := sidebarAccount{Account: acc, Color: accountColor(acc.ID)}
		for _, f := range folders {
			if f.AccountID != acc.ID || !f.Enabled {
				continue
			}
			u := unread[f.ID]
			if u == 0 {
				u = f.UnreadCount
			}
			item.Folders = append(item.Folders, sidebarFolder{
				Folder: f,
				Unread: u,
				Active: f.ID == activeFolder,
			})
			item.Unread += u
		}
		out = append(out, item)
	}
	return out
}

func (s *Server) handleSettingsAccounts(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 15*time.Second)
	defer cancel()
	accounts, err := s.st.ListAccounts(ctx)
	if err != nil {
		s.internalError(w, r, "加载账号失败", err)
		return
	}
	stats, _ := s.st.Stats(ctx)
	folders, _ := s.st.ListFolders(ctx, "")
	unread, _ := s.st.UnreadCounts(ctx)
	s.renderPage(w, r, "settings_accounts.html", pageData{
		Title:     "账号设置 · zMail",
		Section:   "accounts",
		Accounts:  accounts,
		Stats:     stats,
		Providers: mail.Providers(),
		Sidebar:   buildSidebar(accounts, folders, unread, "", ""),
	})
}

func (s *Server) handleSettingsGeneral(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 15*time.Second)
	defer cancel()
	stats, err := s.st.Stats(ctx)
	if err != nil {
		s.log.Warn("统计失败", "err", err)
	}
	accounts, _ := s.st.ListAccounts(ctx)
	folders, _ := s.st.ListFolders(ctx, "")
	unread, _ := s.st.UnreadCounts(ctx)
	s.renderPage(w, r, "settings_general.html", pageData{
		Title:    "通用设置 · zMail",
		Section:  "general",
		Stats:    stats,
		Accounts: accounts,
		Sidebar:  buildSidebar(accounts, folders, unread, "", ""),
	})
}

func isNotFound(err error) bool {
	return errors.Is(err, store.ErrNotFound)
}
