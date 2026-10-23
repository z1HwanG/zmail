package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	netmail "net/mail"
	"strings"
	"time"

	"zmail/internal/mail"
	"zmail/internal/realtime"
	"zmail/internal/store"
)

// accountEndpoint 描述 IMAP/SMTP 连接参数。
type accountEndpoint struct {
	Host   string `json:"host"`
	Port   int    `json:"port"`
	Secure bool   `json:"secure"`
}

// accountDTO 是账号的对外表示，绝不包含密码或密文。
type accountDTO struct {
	ID           string          `json:"id"`
	AccountID    string          `json:"accountId"`
	Email        string          `json:"email"`
	DisplayName  string          `json:"displayName"`
	Provider     string          `json:"provider"`
	Username     string          `json:"username"`
	IMAP         accountEndpoint `json:"imap"`
	SMTP         accountEndpoint `json:"smtp"`
	Status       string          `json:"status"`
	LastError    string          `json:"lastError,omitempty"`
	LastSyncedAt *time.Time      `json:"lastSyncedAt,omitempty"`
	CreatedAt    time.Time       `json:"createdAt"`
}

func accountToDTO(a *store.Account) accountDTO {
	return accountDTO{
		ID:           a.ID,
		AccountID:    a.ID,
		Email:        a.Email,
		DisplayName:  a.DisplayName,
		Provider:     a.Provider,
		Username:     a.Username,
		IMAP:         accountEndpoint{Host: a.IMAPHost, Port: a.IMAPPort, Secure: a.IMAPSecure},
		SMTP:         accountEndpoint{Host: a.SMTPHost, Port: a.SMTPPort, Secure: a.SMTPSecure},
		Status:       a.Status,
		LastError:    a.LastError,
		LastSyncedAt: a.LastSyncedAt,
		CreatedAt:    a.CreatedAt,
	}
}

// accountRequest 是新增账号的请求体。
type accountRequest struct {
	Provider    string `json:"provider"`
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
	Username    string `json:"username"`
	Password    string `json:"password"`
	IMAPHost    string `json:"imapHost"`
	IMAPPort    *int   `json:"imapPort"`
	IMAPSecure  *bool  `json:"imapSecure"`
	SMTPHost    string `json:"smtpHost"`
	SMTPPort    *int   `json:"smtpPort"`
	SMTPSecure  *bool  `json:"smtpSecure"`
}

func (s *Server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 10*time.Second)
	defer cancel()
	accounts, err := s.st.ListAccounts(ctx)
	if err != nil {
		s.internalError(w, r, "查询账号列表失败", err)
		return
	}
	out := make([]accountDTO, 0, len(accounts))
	for _, a := range accounts {
		out = append(out, accountToDTO(a))
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

func (s *Server) handleCreateAccount(w http.ResponseWriter, r *http.Request) {
	var req accountRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "请求体格式错误: "+err.Error())
		return
	}

	acc, err := s.buildAccount(&req)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}

	ctx, cancel := contextWithTimeout(r, 10*time.Second)
	defer cancel()
	if existing, err := s.st.GetAccountByEmail(ctx, acc.Email); err == nil && existing != nil {
		writeError(w, r, http.StatusConflict, codeConflict, "该邮箱账号已存在")
		return
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, r, "检查账号是否重复失败", err)
		return
	}
	if err := s.st.CreateAccount(ctx, acc); err != nil {
		if errors.Is(err, store.ErrInvalid) {
			writeError(w, r, http.StatusBadRequest, codeBadRequest, err.Error())
			return
		}
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			writeError(w, r, http.StatusConflict, codeConflict, "该邮箱账号已存在")
			return
		}
		s.internalError(w, r, "保存账号失败", err)
		return
	}
	s.log.Info("账号已添加", "account_id", acc.ID, "email", acc.Email, "provider", acc.Provider)

	// 异步加载连接并发现文件夹，失败只记日志，不阻塞创建请求。
	s.afterAccountCreated(acc.ID)
	writeJSON(w, http.StatusCreated, map[string]any{"account": accountToDTO(acc)})
}

// buildAccount 校验请求并构造 store.Account（密码在此加密，绝不落明文）。
func (s *Server) buildAccount(req *accountRequest) (*store.Account, error) {
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if email == "" {
		return nil, errors.New("邮箱地址不能为空")
	}
	addr, err := netmail.ParseAddress(email)
	if err != nil || !strings.EqualFold(addr.Address, email) {
		return nil, errors.New("邮箱地址格式不正确")
	}
	password := req.Password
	if password == "" {
		return nil, errors.New("密码或应用专用密码不能为空")
	}

	providerKey := strings.ToLower(strings.TrimSpace(req.Provider))
	if providerKey == "" {
		providerKey = guessProvider(email)
	}
	prov, known := mail.LookupProvider(providerKey)
	if !known && providerKey != "imap" && providerKey != "custom" {
		return nil, fmt.Errorf("未知 provider %q，请使用 imap 或手动填写服务器地址", providerKey)
	}

	imapHost := strings.TrimSpace(req.IMAPHost)
	smtpHost := strings.TrimSpace(req.SMTPHost)
	imapPort := 993
	smtpPort := 465
	imapSecure := true
	smtpSecure := true
	if known {
		if imapHost == "" {
			imapHost = prov.IMAPHost
		}
		if smtpHost == "" {
			smtpHost = prov.SMTPHost
		}
		if prov.IMAPPort > 0 {
			imapPort = prov.IMAPPort
		}
		if prov.SMTPPort > 0 {
			smtpPort = prov.SMTPPort
		}
		imapSecure = prov.IMAPPort == 993
		smtpSecure = prov.SMTPSecure
	}
	if req.IMAPPort != nil && *req.IMAPPort > 0 {
		imapPort = *req.IMAPPort
		imapSecure = imapPort == 993
	}
	if req.SMTPPort != nil && *req.SMTPPort > 0 {
		smtpPort = *req.SMTPPort
		smtpSecure = smtpPort == 465
	}
	if req.IMAPSecure != nil {
		imapSecure = *req.IMAPSecure
	}
	if req.SMTPSecure != nil {
		smtpSecure = *req.SMTPSecure
	}
	if imapHost == "" {
		return nil, errors.New("IMAP 主机不能为空")
	}
	if smtpHost == "" {
		return nil, errors.New("SMTP 主机不能为空")
	}
	if imapPort < 1 || imapPort > 65535 || smtpPort < 1 || smtpPort > 65535 {
		return nil, errors.New("端口必须在 1-65535 之间")
	}
	username := strings.TrimSpace(req.Username)
	if username == "" {
		username = email
	}
	displayName := strings.TrimSpace(req.DisplayName)
	if displayName == "" {
		displayName = email
	}
	enc, err := s.box.EncryptString(password)
	if err != nil {
		return nil, errors.New("加密账号密码失败")
	}
	acc := &store.Account{
		ID:          store.NewID(),
		Provider:    providerKey,
		Email:       email,
		DisplayName: displayName,
		Username:    username,
		PasswordEnc: enc,
		IMAPHost:    imapHost,
		IMAPPort:    imapPort,
		IMAPSecure:  imapSecure,
		SMTPHost:    smtpHost,
		SMTPPort:    smtpPort,
		SMTPSecure:  smtpSecure,
		Status:      store.AccountActive,
		CreatedAt:   time.Now().UTC(),
	}
	if err := acc.Validate(); err != nil {
		return nil, errors.New("账号信息不完整: " + err.Error())
	}
	return acc, nil
}

func guessProvider(email string) string {
	at := strings.LastIndex(email, "@")
	if at < 0 {
		return "imap"
	}
	domain := strings.ToLower(email[at+1:])
	switch {
	case domain == "gmail.com", domain == "googlemail.com":
		return "gmail"
	case domain == "outlook.com", domain == "hotmail.com", domain == "live.com",
		domain == "msn.com", strings.HasPrefix(domain, "outlook."):
		return "outlook"
	case domain == "qq.com", domain == "foxmail.com", domain == "vip.qq.com":
		return "qq"
	case domain == "163.com", domain == "126.com", domain == "yeah.net", domain == "vip.163.com":
		return "163"
	default:
		return "imap"
	}
}

func (s *Server) afterAccountCreated(accountID string) {
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("账号初始化协程 panic", "account_id", accountID, "panic", rec)
			}
		}()
		if s.mail == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		s.mail.ReloadAccount(ctx, accountID)
		folders, err := s.mail.DiscoverFolders(ctx, accountID)
		if err != nil {
			s.log.Warn("发现文件夹失败", "account_id", accountID, "err", err)
			_ = s.st.AddSyncLog(context.Background(), accountID, "warn", "发现文件夹失败: "+err.Error())
			return
		}
		s.log.Info("账号文件夹已发现", "account_id", accountID, "folders", len(folders))
	}()
}

func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx, cancel := contextWithTimeout(r, 15*time.Second)
	defer cancel()
	if _, err := s.st.GetAccount(ctx, id); err != nil {
		s.storeError(w, r, "账号不存在", err)
		return
	}
	if s.mail != nil {
		s.mail.RemoveAccount(id)
	}
	if err := s.st.DeleteAccount(ctx, id); err != nil {
		s.internalError(w, r, "删除账号失败", err)
		return
	}
	s.log.Info("账号已删除", "account_id", id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleTestAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.mail == nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "邮件引擎未就绪")
		return
	}
	ctx, cancel := contextWithTimeout(r, 35*time.Second)
	defer cancel()
	acc, err := s.st.GetAccount(ctx, id)
	if err != nil {
		s.storeError(w, r, "账号不存在", err)
		return
	}
	if err := s.mail.TestConnection(ctx, acc); err != nil {
		_ = s.st.SetAccountStatus(context.Background(), id, store.AccountAuthFailed, err.Error())
		s.log.Warn("账号连接测试失败", "account_id", id, "err", err)
		writeError(w, r, http.StatusBadGateway, codeUpstreamError, "连接测试失败: "+err.Error())
		return
	}
	_ = s.st.SetAccountStatus(context.Background(), id, store.AccountActive, "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": store.AccountActive})
}

func (s *Server) handleSyncAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.mail == nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "邮件引擎未就绪")
		return
	}
	ctx, cancel := contextWithTimeout(r, 10*time.Second)
	defer cancel()
	if _, err := s.st.GetAccount(ctx, id); err != nil {
		s.storeError(w, r, "账号不存在", err)
		return
	}
	s.hub.Publish(realtime.EventSyncStatus, realtime.SyncStatus{AccountID: id, Status: "syncing", Message: "正在同步"})
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("同步协程 panic", "account_id", id, "panic", rec)
			}
		}()
		syncCtx, syncCancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer syncCancel()
		if err := s.mail.SyncAccount(syncCtx, id); err != nil {
			s.log.Warn("手动同步失败", "account_id", id, "err", err)
			_ = s.st.AddSyncLog(context.Background(), id, "error", "手动同步失败: "+err.Error())
			_ = s.st.SetAccountStatus(context.Background(), id, store.AccountActive, err.Error())
			s.hub.Publish(realtime.EventSyncStatus, realtime.SyncStatus{AccountID: id, Status: "error", Message: err.Error()})
			return
		}
		_ = s.st.TouchAccount(context.Background(), id)
		s.hub.Publish(realtime.EventSyncStatus, realtime.SyncStatus{AccountID: id, Status: "idle", Message: "同步完成"})
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "status": "syncing"})
}

// accountUpdateRequest 是修改账号的请求体：字段用指针区分“未提供”和“显式设为空”。
type accountUpdateRequest struct {
	Provider    *string `json:"provider"`
	Email       *string `json:"email"`
	DisplayName *string `json:"displayName"`
	Username    *string `json:"username"`
	Password    *string `json:"password"`
	IMAPHost    *string `json:"imapHost"`
	IMAPPort    *int    `json:"imapPort"`
	IMAPSecure  *bool   `json:"imapSecure"`
	SMTPHost    *string `json:"smtpHost"`
	SMTPPort    *int    `json:"smtpPort"`
	SMTPSecure  *bool   `json:"smtpSecure"`
	Status      *string `json:"status"`
}

func (s *Server) handleGetAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx, cancel := contextWithTimeout(r, 10*time.Second)
	defer cancel()
	acc, err := s.st.GetAccount(ctx, id)
	if err != nil {
		s.storeError(w, r, "账号不存在", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"account": accountToDTO(acc)})
}

// handleUpdateAccount 修改已添加账号的连接参数、显示名或密码。
// 密码留空表示保留原密码；切换 status=active/disabled 可启停账号。
func (s *Server) handleUpdateAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req accountUpdateRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "请求体格式错误: "+err.Error())
		return
	}

	ctx, cancel := contextWithTimeout(r, 15*time.Second)
	defer cancel()
	acc, err := s.st.GetAccount(ctx, id)
	if err != nil {
		s.storeError(w, r, "账号不存在", err)
		return
	}

	if err := s.applyAccountUpdate(acc, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	if err := s.st.UpdateAccount(ctx, acc); err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeError(w, r, http.StatusNotFound, codeNotFound, "账号不存在")
		case errors.Is(err, store.ErrInvalid):
			if strings.Contains(strings.ToLower(err.Error()), "已存在") {
				writeError(w, r, http.StatusConflict, codeConflict, "该邮箱已被其他账号使用")
				return
			}
			writeError(w, r, http.StatusBadRequest, codeBadRequest, err.Error())
		default:
			s.internalError(w, r, "更新账号失败", err)
		}
		return
	}
	s.log.Info("账号已更新", "account_id", acc.ID, "email", acc.Email, "status", acc.Status)

	// 配置变更后重启该账号的 IDLE 连接；停用则直接断开。
	if s.mail != nil {
		if acc.Status == store.AccountDisabled {
			s.mail.RemoveAccount(acc.ID)
		} else {
			s.afterAccountUpdated(acc.ID)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"account": accountToDTO(acc)})
}

// applyAccountUpdate 把请求中的字段合并进账号，并做完整校验；密码在此加密。
func (s *Server) applyAccountUpdate(acc *store.Account, req *accountUpdateRequest) error {
	if req.Provider != nil {
		key := strings.ToLower(strings.TrimSpace(*req.Provider))
		if key != "" {
			if _, known := mail.LookupProvider(key); !known && key != "imap" && key != "custom" {
				return fmt.Errorf("未知 provider %q", key)
			}
			acc.Provider = key
			// 切到已知服务商且未显式指定服务器时，套用该服务商默认参数。
			if prov, ok := mail.LookupProvider(key); ok {
				if req.IMAPHost == nil {
					acc.IMAPHost = prov.IMAPHost
					acc.IMAPPort = prov.IMAPPort
					acc.IMAPSecure = prov.IMAPPort == 993
				}
				if req.SMTPHost == nil {
					acc.SMTPHost = prov.SMTPHost
					acc.SMTPPort = prov.SMTPPort
					acc.SMTPSecure = prov.SMTPSecure
				}
			}
		}
	}
	if req.Email != nil {
		email := strings.ToLower(strings.TrimSpace(*req.Email))
		if email == "" {
			return errors.New("邮箱地址不能为空")
		}
		addr, err := netmail.ParseAddress(email)
		if err != nil || !strings.EqualFold(addr.Address, email) {
			return errors.New("邮箱地址格式不正确")
		}
		acc.Email = email
	}
	if req.DisplayName != nil {
		name := strings.TrimSpace(*req.DisplayName)
		if name == "" {
			name = acc.Email
		}
		acc.DisplayName = name
	}
	if req.Username != nil {
		user := strings.TrimSpace(*req.Username)
		if user == "" {
			user = acc.Email
		}
		acc.Username = user
	}
	if req.Password != nil && *req.Password != "" {
		enc, err := s.box.EncryptString(*req.Password)
		if err != nil {
			return errors.New("加密账号密码失败")
		}
		acc.PasswordEnc = enc
	}
	if req.IMAPHost != nil {
		host := strings.TrimSpace(*req.IMAPHost)
		if host == "" {
			return errors.New("IMAP 主机不能为空")
		}
		acc.IMAPHost = host
	}
	if req.IMAPPort != nil && *req.IMAPPort != 0 {
		if *req.IMAPPort < 1 || *req.IMAPPort > 65535 {
			return errors.New("IMAP 端口必须在 1-65535 之间")
		}
		acc.IMAPPort = *req.IMAPPort
	}
	if req.IMAPSecure != nil {
		acc.IMAPSecure = *req.IMAPSecure
	}
	if req.SMTPHost != nil {
		host := strings.TrimSpace(*req.SMTPHost)
		if host == "" {
			return errors.New("SMTP 主机不能为空")
		}
		acc.SMTPHost = host
	}
	if req.SMTPPort != nil && *req.SMTPPort != 0 {
		if *req.SMTPPort < 1 || *req.SMTPPort > 65535 {
			return errors.New("SMTP 端口必须在 1-65535 之间")
		}
		acc.SMTPPort = *req.SMTPPort
	}
	if req.SMTPSecure != nil {
		acc.SMTPSecure = *req.SMTPSecure
	}
	if req.Status != nil {
		switch strings.ToLower(strings.TrimSpace(*req.Status)) {
		case store.AccountActive:
			acc.Status = store.AccountActive
			acc.LastError = ""
		case store.AccountDisabled:
			acc.Status = store.AccountDisabled
		default:
			return errors.New("status 只能是 active 或 disabled")
		}
	}
	if acc.IMAPPort == 0 {
		acc.IMAPPort = 993
	}
	if acc.SMTPPort == 0 {
		acc.SMTPPort = 465
	}
	if err := acc.Validate(); err != nil {
		return errors.New("账号信息不完整: " + err.Error())
	}
	return nil
}

// afterAccountUpdated 在配置更新后重启 watcher 并重新发现文件夹。
func (s *Server) afterAccountUpdated(accountID string) {
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("账号更新协程 panic", "account_id", accountID, "panic", rec)
			}
		}()
		if s.mail == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		s.mail.ReloadAccount(ctx, accountID)
		if _, err := s.mail.DiscoverFolders(ctx, accountID); err != nil {
			s.log.Warn("更新后重新发现文件夹失败", "account_id", accountID, "err", err)
			_ = s.st.AddSyncLog(context.Background(), accountID, "warn", "更新后重新发现文件夹失败: "+err.Error())
		}
	}()
}

// ---------- 错误映射辅助 ----------

func (s *Server) internalError(w http.ResponseWriter, r *http.Request, msg string, err error) {
	s.log.Error(msg, "err", err, "path", r.URL.Path)
	writeError(w, r, http.StatusInternalServerError, codeInternal, msg)
}

func (s *Server) storeError(w http.ResponseWriter, r *http.Request, notFoundMsg string, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, r, http.StatusNotFound, codeNotFound, notFoundMsg)
	case errors.Is(err, store.ErrInvalid):
		writeError(w, r, http.StatusBadRequest, codeBadRequest, err.Error())
	default:
		s.internalError(w, r, "存储操作失败", err)
	}
}
