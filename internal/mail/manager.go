package mail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emersion/go-imap"

	"zmail/internal/config"
	"zmail/internal/realtime"
	"zmail/internal/secret"
	"zmail/internal/store"
)

// 账号运行状态（与 AccountStatus.State 对应）。
const (
	StateDisconnected = "disconnected"
	StateConnecting   = "connecting"
	StateAuth         = "auth"
	StateSelect       = "select"
	StateCatchup      = "catchup"
	StateIdle         = "idle"
	StateSyncing      = "syncing"
	StateError        = "error"
	StateAuthFailed   = "auth_failed"
	StateDisabled     = "disabled"
)

// 任务类型。
const (
	JobRetrySend    = "retry_send"
	JobRetrySync    = "retry_sync"
	JobBackfill     = "backfill"
	JobDeleteRemote = "delete_remote"
)

// 失败重试的默认参数。
const (
	jobPollInterval   = 30 * time.Second
	idlePollInterval  = 13 * time.Second // 不支持 IDLE 时的轮询间隔（12~15 秒）
	reconnectMinDelay = time.Second
	reconnectMaxDelay = 60 * time.Second
)

var (
	errInvalidAccount  = errors.New("mail: 账号参数不合法")
	errPrivateHost     = errors.New("mail: 拒绝连接内网地址")
	errAccountDisabled = errors.New("mail: 账号已禁用")
	errManagerStopped  = errors.New("mail: 邮件引擎已停止")
)

// SendRequest 是一封待发送邮件的参数。
type SendRequest struct {
	AccountID   string
	To          []string
	Cc          []string
	Bcc         []string
	Subject     string
	TextBody    string
	HTMLBody    string
	InReplyTo   string
	References  []string
	Attachments []Attachment
}

// Attachment 是发信时的附件内容。
type Attachment struct {
	Filename string
	MimeType string
	Data     []byte
}

// AccountStatus 是单个账号的运行状态快照。
type AccountStatus struct {
	AccountID   string    `json:"accountId"`
	State       string    `json:"state"`
	UsingIDLE   bool      `json:"usingIdle"`
	LastError   string    `json:"lastError,omitempty"`
	LastEventAt time.Time `json:"lastEventAt"`
}

// Manager 负责账号生命周期、IMAP 实时监听、增量同步与发信。
type Manager struct {
	cfg *config.Config
	st  *store.Store
	box *secret.Box
	hub *realtime.Hub
	log *slog.Logger

	ctx         context.Context
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	started     atomic.Bool
	startedOnce sync.Once
	stopped     sync.Once

	fetchSem chan struct{}
	jobsKick chan struct{}

	mu       sync.RWMutex
	accounts map[string]*accountRuntime

	syncMu    sync.Mutex
	syncLocks map[string]*sync.Mutex
	// lastBackfill 记录最近一次回填插入数，用于自适应降速。
	lastBackfill int
}

// accountRuntime 是一个账号的后台运行态：IDLE 监听、同步信号与状态。
type accountRuntime struct {
	id     string
	cancel context.CancelFunc
	wg     sync.WaitGroup
	syncCh chan struct{}

	mu     sync.Mutex
	status AccountStatus
}

func (rt *accountRuntime) setState(state string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.status.State == StateAuthFailed && state != StateAuthFailed {
		// 认证失败后不再被普通状态覆盖，直到账号重载。
		return
	}
	rt.status.State = state
	rt.status.LastEventAt = time.Now()
}

func (rt *accountRuntime) setIDLE(using bool) {
	rt.mu.Lock()
	rt.status.UsingIDLE = using
	rt.mu.Unlock()
}

func (rt *accountRuntime) setError(state, message string) {
	rt.mu.Lock()
	rt.status.State = state
	rt.status.LastError = message
	rt.status.LastEventAt = time.Now()
	rt.mu.Unlock()
}

func (rt *accountRuntime) touch() {
	rt.mu.Lock()
	rt.status.LastEventAt = time.Now()
	rt.mu.Unlock()
}

func (rt *accountRuntime) snapshot() AccountStatus {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.status
}

// NewManager 创建邮件引擎。所有依赖由调用方注入，Manager 不持有连接直到 Start。
func NewManager(cfg *config.Config, st *store.Store, box *secret.Box, hub *realtime.Hub, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	concurrency := 1
	if cfg != nil && cfg.MaxFetchConcurrency > 0 {
		concurrency = cfg.MaxFetchConcurrency
	}
	return &Manager{
		cfg:       cfg,
		st:        st,
		box:       box,
		hub:       hub,
		log:       log,
		fetchSem:  make(chan struct{}, concurrency),
		jobsKick:  make(chan struct{}, 1),
		accounts:  make(map[string]*accountRuntime),
		syncLocks: make(map[string]*sync.Mutex),
	}
}

// Start 启动所有账号的监听与后台任务。重复调用是幂等的。
func (m *Manager) Start(ctx context.Context) {
	if m == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.startedOnce.Do(func() {
		m.ctx, m.cancel = context.WithCancel(ctx)
		m.started.Store(true)

		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			m.jobsLoop(m.ctx)
		}()
		if m.cfg != nil && m.cfg.BackfillEnabled {
			m.wg.Add(1)
			go func() {
				defer m.wg.Done()
				m.backfillLoop(m.ctx)
			}()
		}

		accounts, err := m.st.ListAccounts(m.ctx)
		if err != nil {
			m.log.Error("加载账号列表失败", "error", err)
			return
		}
		for _, acc := range accounts {
			m.startAccount(acc)
		}
	})
}

// Stop 取消所有后台任务并等待 goroutine 退出，返回后不留泄漏。
func (m *Manager) Stop() {
	if m == nil {
		return
	}
	m.stopped.Do(func() {
		if m.cancel != nil {
			m.cancel()
		}
		m.mu.RLock()
		runtimes := make([]*accountRuntime, 0, len(m.accounts))
		for _, rt := range m.accounts {
			runtimes = append(runtimes, rt)
		}
		m.mu.RUnlock()
		for _, rt := range runtimes {
			rt.cancel()
		}
		m.wg.Wait()
		for _, rt := range runtimes {
			rt.wg.Wait()
		}
		m.started.Store(false)
	})
}

// ReloadAccount 重新加载账号配置：先停掉旧的后台任务，再按最新配置启动。
func (m *Manager) ReloadAccount(ctx context.Context, accountID string) {
	if m == nil || accountID == "" || m.st == nil {
		return
	}
	m.removeRuntime(accountID)
	if !m.started.Load() {
		return
	}
	acc, err := m.st.GetAccount(ctx, accountID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			m.log.Warn("重新加载账号失败", "account", accountID, "error", err)
		}
		return
	}
	m.startAccount(acc)
}

// RemoveAccount 停止并移除账号的后台任务。
func (m *Manager) RemoveAccount(accountID string) {
	if m == nil || accountID == "" {
		return
	}
	m.removeRuntime(accountID)
}

// startAccount 为账号启动同步 worker（以及未禁用/未认证失败时的 IDLE watcher）。
func (m *Manager) startAccount(acc *store.Account) {
	if acc == nil || acc.ID == "" {
		return
	}
	if !m.started.Load() || m.ctx == nil {
		return
	}

	ctx, cancel := context.WithCancel(m.ctx)
	rt := &accountRuntime{
		id:     acc.ID,
		cancel: cancel,
		syncCh: make(chan struct{}, 1),
	}
	rt.status = AccountStatus{
		AccountID:   acc.ID,
		State:       StateDisconnected,
		LastEventAt: time.Now(),
	}

	m.mu.Lock()
	old := m.accounts[acc.ID]
	m.accounts[acc.ID] = rt
	m.mu.Unlock()
	if old != nil {
		m.stopRuntime(old)
	}

	rt.wg.Add(1)
	m.wg.Add(1)
	go func() {
		defer rt.wg.Done()
		defer m.wg.Done()
		m.syncWorker(ctx, rt)
	}()

	if acc.Status == store.AccountAuthFailed {
		rt.setError(StateAuthFailed, "IMAP 认证失败，请更新授权码后重新连接")
		return
	}
	if acc.Status == store.AccountDisabled {
		rt.setState(StateDisabled)
		return
	}

	rt.wg.Add(1)
	m.wg.Add(1)
	go func() {
		defer rt.wg.Done()
		defer m.wg.Done()
		m.idleWatcher(ctx, rt, acc)
	}()
}

// removeRuntime 从状态表摘除并停止账号后台任务。
func (m *Manager) removeRuntime(accountID string) {
	m.mu.Lock()
	rt := m.accounts[accountID]
	delete(m.accounts, accountID)
	m.mu.Unlock()
	if rt != nil {
		m.stopRuntime(rt)
	}
}

func (m *Manager) stopRuntime(rt *accountRuntime) {
	if rt == nil {
		return
	}
	rt.cancel()
	rt.wg.Wait()
}

// runtime 返回账号运行态（可能为 nil）。
func (m *Manager) runtime(accountID string) *accountRuntime {
	m.mu.RLock()
	rt := m.accounts[accountID]
	m.mu.RUnlock()
	return rt
}

// syncWorker 串行消费同步信号：同一账号的同步请求会合并，不会并发跑多条连接。
func (m *Manager) syncWorker(ctx context.Context, rt *accountRuntime) {
	for {
		select {
		case <-ctx.Done():
			rt.setState(StateDisconnected)
			return
		case <-rt.syncCh:
		}
		acc, err := m.st.GetAccount(ctx, rt.id)
		if err != nil {
			if ctx.Err() == nil {
				m.log.Warn("同步前读取账号失败", "account", rt.id, "error", err)
			}
			continue
		}
		if acc.Status == store.AccountDisabled {
			rt.setState(StateDisabled)
			return
		}
		if err := m.syncAccount(ctx, acc, 0); err != nil && ctx.Err() == nil {
			m.log.Warn("账号同步失败", "account", acc.ID, "error", err)
		}
	}
}

// notifySync 向账号的同步 worker 投递一次非阻塞同步信号。
func (m *Manager) notifySync(accountID string) {
	rt := m.runtime(accountID)
	if rt == nil {
		return
	}
	select {
	case rt.syncCh <- struct{}{}:
	default:
	}
}

// accountLock 返回账号级互斥锁，避免同一账号被 IDLE、手动同步、回填并发拉取。
func (m *Manager) accountLock(accountID string) func() {
	m.syncMu.Lock()
	mu := m.syncLocks[accountID]
	if mu == nil {
		mu = &sync.Mutex{}
		m.syncLocks[accountID] = mu
	}
	m.syncMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

// acquireFetch 获取一个全局拉取并发槽位。
func (m *Manager) acquireFetch(ctx context.Context) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case m.fetchSem <- struct{}{}:
		return func() { <-m.fetchSem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// fetchBusy 报告当前是否有实时拉取任务在跑（供回填降速）。
func (m *Manager) fetchBusy() bool {
	return len(m.fetchSem) > 0
}

// SyncAccount 立即同步一个账号的全部启用文件夹。
func (m *Manager) SyncAccount(ctx context.Context, accountID string) error {
	if m == nil || m.st == nil {
		return errManagerStopped
	}
	if accountID == "" {
		return fmt.Errorf("%w: 账号 ID 为空", errInvalidAccount)
	}
	acc, err := m.st.GetAccount(ctx, accountID)
	if err != nil {
		return err
	}
	if acc.Status == store.AccountDisabled {
		return fmt.Errorf("%w: 账号 %s", errAccountDisabled, accountID)
	}
	return m.syncAccount(ctx, acc, 0)
}

// syncAccount 顺序同步账号下所有启用文件夹（收件箱优先）。
func (m *Manager) syncAccount(ctx context.Context, acc *store.Account, limit int) error {
	unlock := m.accountLock(acc.ID)
	defer unlock()

	release, err := m.acquireFetch(ctx)
	if err != nil {
		return err
	}
	defer release()

	if rt := m.runtime(acc.ID); rt != nil {
		rt.setState(StateSyncing)
	}
	m.publishSync(acc.ID, "syncing", "")

	folders, err := m.st.ListFolders(ctx, acc.ID)
	if err != nil {
		return fmt.Errorf("mail: 读取账号文件夹失败: %w", err)
	}
	// 新账号还没有文件夹记录时，先发现一次文件夹再同步。
	if len(folders) == 0 {
		if _, derr := m.DiscoverFolders(ctx, acc.ID); derr != nil {
			m.log.Warn("发现账号文件夹失败", "account", acc.ID, "error", derr)
		} else if folders, err = m.st.ListFolders(ctx, acc.ID); err != nil {
			return fmt.Errorf("mail: 读取账号文件夹失败: %w", err)
		}
	}
	sort.SliceStable(folders, func(i, j int) bool {
		return folderPriority(folders[i].Role) < folderPriority(folders[j].Role)
	})

	var firstErr error
	for _, f := range folders {
		if !f.Enabled {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := m.syncFolder(ctx, acc, f, limit); err != nil {
			if isAuthError(err) {
				m.markAuthFailed(ctx, acc, err)
				return err
			}
			if firstErr == nil {
				firstErr = fmt.Errorf("mail: 同步文件夹 %s 失败: %w", f.RemotePath, err)
			}
			m.log.Warn("同步文件夹失败", "account", acc.ID, "folder", f.RemotePath, "error", err)
		}
	}

	if err := m.st.TouchAccount(ctx, acc.ID); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("mail: 更新账号同步时间失败: %w", err)
	}
	if firstErr != nil {
		m.publishSync(acc.ID, "error", "部分文件夹同步失败")
		if rt := m.runtime(acc.ID); rt != nil {
			rt.setError(StateError, "部分文件夹同步失败")
		}
		return firstErr
	}
	m.publishSync(acc.ID, "idle", "")
	if rt := m.runtime(acc.ID); rt != nil {
		rt.setState(StateIdle)
	}
	return nil
}

// SyncFolder 立即同步单个文件夹，返回新插入的邮件数。
func (m *Manager) SyncFolder(ctx context.Context, folderID string) (int, error) {
	if m == nil || m.st == nil {
		return 0, errManagerStopped
	}
	if folderID == "" {
		return 0, fmt.Errorf("mail: 文件夹 ID 为空")
	}
	f, err := m.st.GetFolder(ctx, folderID)
	if err != nil {
		return 0, err
	}
	acc, err := m.st.GetAccount(ctx, f.AccountID)
	if err != nil {
		return 0, err
	}
	unlock := m.accountLock(acc.ID)
	defer unlock()
	release, err := m.acquireFetch(ctx)
	if err != nil {
		return 0, err
	}
	defer release()
	return m.syncFolder(ctx, acc, f, 0)
}

// Statuses 返回全部账号的状态快照（按账号 ID 排序保证输出稳定）。
func (m *Manager) Statuses() []AccountStatus {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	ids := make([]string, 0, len(m.accounts))
	rts := make(map[string]*accountRuntime, len(m.accounts))
	for id, rt := range m.accounts {
		ids = append(ids, id)
		rts[id] = rt
	}
	m.mu.RUnlock()
	sort.Strings(ids)
	out := make([]AccountStatus, 0, len(ids))
	for _, id := range ids {
		out = append(out, rts[id].snapshot())
	}
	return out
}

// Status 返回单个账号的状态快照；未知账号返回 disconnected。
func (m *Manager) Status(accountID string) AccountStatus {
	if m == nil {
		return AccountStatus{AccountID: accountID, State: StateDisconnected}
	}
	if rt := m.runtime(accountID); rt != nil {
		return rt.snapshot()
	}
	return AccountStatus{AccountID: accountID, State: StateDisconnected, LastEventAt: time.Now()}
}

// markAuthFailed 标记账号认证失败并停止其后台重试。
func (m *Manager) markAuthFailed(ctx context.Context, acc *store.Account, cause error) {
	msg := "IMAP 认证失败，请更新授权码后重新连接"
	m.log.Warn("账号认证失败，停止重试", "account", acc.ID, "error", cause)
	if m.st != nil {
		if err := m.st.SetAccountStatus(ctx, acc.ID, store.AccountAuthFailed, msg); err != nil {
			m.log.Warn("写入账号认证失败状态失败", "account", acc.ID, "error", err)
		}
		m.syncLog(ctx, acc.ID, "error", msg)
	}
	if rt := m.runtime(acc.ID); rt != nil {
		rt.setError(StateAuthFailed, msg)
		rt.cancel()
	}
	m.publishSync(acc.ID, "error", msg)
}

// jobsLoop 轮询持久化任务（发信重试、同步重试、回填）。
func (m *Manager) jobsLoop(ctx context.Context) {
	ticker := time.NewTicker(jobPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-m.jobsKick:
		}
		m.processJobs(ctx)
	}
}

func (m *Manager) processJobs(ctx context.Context) {
	jobs, err := m.st.DueJobs(ctx, 20)
	if err != nil {
		if ctx.Err() == nil {
			m.log.Warn("查询待执行任务失败", "error", err)
		}
		return
	}
	for _, job := range jobs {
		if ctx.Err() != nil {
			return
		}
		if err := m.st.ClaimJob(ctx, job.ID); err != nil {
			continue
		}
		jobErr := m.runJob(ctx, job)
		if err := m.st.FinishJob(ctx, job.ID, jobErr); err != nil {
			m.log.Warn("结束任务失败", "job", job.ID, "error", err)
		}
	}
}

func (m *Manager) runJob(ctx context.Context, job *store.Job) error {
	switch job.Kind {
	case JobRetrySend:
		return m.SendQueued(ctx, job)
	case JobRetrySync:
		accountID := job.AccountID
		if accountID == "" {
			var payload struct {
				AccountID string `json:"accountId"`
			}
			if job.Payload != "" {
				_ = json.Unmarshal([]byte(job.Payload), &payload)
			}
			accountID = payload.AccountID
		}
		if accountID == "" {
			return fmt.Errorf("mail: 同步重试任务缺少账号 ID")
		}
		return m.SyncAccount(ctx, accountID)
	case JobBackfill:
		return m.runBackfillStep(ctx)
	case JobDeleteRemote:
		return m.runDeleteRemote(ctx, job)
	default:
		m.log.Warn("忽略未知任务类型", "kind", job.Kind)
		return nil
	}
}

// kickJobs 立即唤醒任务轮询（用于刚刚入队的重试任务）。
func (m *Manager) kickJobs() {
	select {
	case m.jobsKick <- struct{}{}:
	default:
	}
}

// publish 向 SSE Hub 广播事件（hub 为空时静默忽略）。
func (m *Manager) publish(name string, data any) {
	if m.hub == nil {
		return
	}
	m.hub.Publish(name, data)
}

func (m *Manager) publishSync(accountID, status, message string) {
	m.publish(realtime.EventSyncStatus, realtime.SyncStatus{
		AccountID: accountID,
		Status:    status,
		Message:   message,
	})
}

func (m *Manager) syncLog(ctx context.Context, accountID, level, message string) {
	if m.st == nil {
		return
	}
	if err := m.st.AddSyncLog(ctx, accountID, level, message); err != nil && ctx.Err() == nil {
		m.log.Debug("写入同步日志失败", "account", accountID, "error", err)
	}
}

// TestConnection 用短超时验证账号的 IMAP 配置（登录并打开收件箱）。
func (m *Manager) TestConnection(ctx context.Context, acc *store.Account) error {
	if acc == nil {
		return fmt.Errorf("%w: 账号为空", errInvalidAccount)
	}
	if acc.IMAPPort == 0 {
		acc.IMAPPort = 993
	}
	if ctx == nil {
		ctx = context.Background()
	}
	conn, err := m.openIMAP(ctx, acc, testConnTimeout)
	if err != nil {
		return err
	}
	if _, selErr := conn.client.Select(imap.InboxName, true); selErr != nil {
		conn.close()
		return fmt.Errorf("mail: 登录成功但无法打开收件箱: %w", selErr)
	}
	conn.close()

	// 顺带验证 SMTP（TLS 握手 + AUTH），失败时给出明确错误。
	if err := m.validateIMAPHost(ctx, acc.SMTPHost); err != nil {
		return err
	}
	pass, err := m.decryptPassword(acc)
	if err != nil {
		return err
	}
	smtpClient, err := newSMTPClient(acc, pass, testConnTimeout)
	if err != nil {
		return err
	}
	if err := smtpClient.DialWithContext(ctx); err != nil {
		return fmt.Errorf("mail: SMTP 连接失败: %w", err)
	}
	_ = smtpClient.Close()
	return nil
}

// TestAccount 测试已保存账号的连接；成功后自动解除 auth_failed 并重启监听。
func (m *Manager) TestAccount(ctx context.Context, accountID string) error {
	if m == nil || m.st == nil {
		return errManagerStopped
	}
	if accountID == "" {
		return fmt.Errorf("%w: 账号 ID 为空", errInvalidAccount)
	}
	acc, err := m.st.GetAccount(ctx, accountID)
	if err != nil {
		return err
	}
	if err := m.TestConnection(ctx, acc); err != nil {
		return err
	}
	if acc.Status == store.AccountAuthFailed {
		if err := m.st.SetAccountStatus(ctx, acc.ID, store.AccountActive, ""); err != nil {
			m.log.Warn("解除账号认证失败状态失败", "account", acc.ID, "error", err)
		}
		m.ReloadAccount(ctx, acc.ID)
	}
	return nil
}

// DiscoverFolders 通过 LIST 发现账号的文件夹，并按名称/属性映射 role。
// Gmail 的 [Gmail]/All Mail 会被跳过；每个账号最多保留一个 role=inbox。
func (m *Manager) DiscoverFolders(ctx context.Context, accountID string) ([]*store.Folder, error) {
	if m == nil || m.st == nil {
		return nil, errManagerStopped
	}
	if accountID == "" {
		return nil, fmt.Errorf("%w: 账号 ID 为空", errInvalidAccount)
	}
	acc, err := m.st.GetAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}

	conn, err := m.openIMAP(ctx, acc, imapDialTimeout)
	if err != nil {
		return nil, err
	}
	defer conn.close()
	conn.client.Timeout = imapOpTimeout

	ch := make(chan *imap.MailboxInfo, 32)
	errCh := make(chan error, 1)
	go func() {
		errCh <- conn.client.List("", "*", ch)
	}()
	infos := make([]*imap.MailboxInfo, 0, 32)
	for info := range ch {
		if info != nil {
			infos = append(infos, info)
		}
	}
	if err := <-errCh; err != nil {
		return nil, fmt.Errorf("mail: LIST 文件夹失败: %w", err)
	}

	out := make([]*store.Folder, 0, len(infos))
	inboxSeen := false
	for _, info := range infos {
		if shouldSkipFolder(acc.Provider, info) {
			continue
		}
		role := folderRole(info)
		if role == store.RoleInbox {
			if inboxSeen {
				role = store.RoleCustom
			} else {
				inboxSeen = true
			}
		}
		f := &store.Folder{
			AccountID:  acc.ID,
			RemotePath: info.Name,
			Name:       displayFolderName(info.Name),
			Role:       role,
			Priority:   folderPriority(role),
			Enabled:    defaultEnabled(role),
		}
		// 已存在的文件夹保留用户的启用状态与优先级；收件箱强制启用。
		if old, gerr := m.st.GetFolderByRemotePath(ctx, acc.ID, info.Name); gerr == nil && old != nil {
			f.Enabled = old.Enabled
			f.Priority = old.Priority
			if role == store.RoleInbox {
				f.Enabled = true
				f.Priority = folderPriority(store.RoleInbox)
			}
		}
		if err := m.st.UpsertFolder(ctx, f); err != nil {
			m.log.Warn("写入文件夹失败", "account", acc.ID, "folder", info.Name, "error", err)
			continue
		}
		out = append(out, f)
	}

	// 确保账号始终有一个收件箱：LIST 没有 INBOX 时补一条。
	if !inboxSeen {
		inbox, gerr := m.st.GetFolderByRole(ctx, acc.ID, store.RoleInbox)
		if gerr != nil || inbox == nil {
			inbox = &store.Folder{
				AccountID:  acc.ID,
				RemotePath: imap.InboxName,
				Name:       imap.InboxName,
				Role:       store.RoleInbox,
				Priority:   folderPriority(store.RoleInbox),
				Enabled:    true,
			}
			if err := m.st.UpsertFolder(ctx, inbox); err != nil {
				return out, fmt.Errorf("mail: 写入收件箱文件夹失败: %w", err)
			}
		}
		out = append(out, inbox)
	}

	// 用 GetFolderByRole 校验唯一 inbox，把多出来的降级为 custom。
	if inbox, gerr := m.st.GetFolderByRole(ctx, acc.ID, store.RoleInbox); gerr == nil && inbox != nil {
		for _, f := range out {
			if f == nil || f.Role != store.RoleInbox || f.ID == inbox.ID {
				continue
			}
			f.Role = store.RoleCustom
			if err := m.st.UpsertFolder(ctx, f); err != nil {
				m.log.Warn("降级重复收件箱失败", "account", acc.ID, "folder", f.RemotePath, "error", err)
			}
		}
	}

	return out, nil
}

// shouldSkipFolder 判断 LIST 结果中应忽略的文件夹（不可选、Gmail 的 All Mail 等）。
func shouldSkipFolder(provider string, info *imap.MailboxInfo) bool {
	if info == nil || strings.TrimSpace(info.Name) == "" {
		return true
	}
	for _, attr := range info.Attributes {
		if strings.EqualFold(attr, imap.NoSelectAttr) {
			return true
		}
	}
	if provider != ProviderGmail {
		return false
	}
	name := strings.ToLower(info.Name)
	if strings.Contains(name, "all mail") || strings.Contains(name, "所有邮件") {
		return true
	}
	for _, attr := range info.Attributes {
		if strings.EqualFold(attr, imap.AllAttr) {
			return true
		}
	}
	return false
}

// folderRole 按 SPECIAL-USE 属性优先、名称启发式兜底映射文件夹角色。
func folderRole(info *imap.MailboxInfo) string {
	if info == nil {
		return store.RoleCustom
	}
	hasAttr := func(target string) bool {
		for _, attr := range info.Attributes {
			if strings.EqualFold(attr, target) {
				return true
			}
		}
		return false
	}
	switch {
	case hasAttr(imap.SentAttr):
		return store.RoleSent
	case hasAttr(imap.DraftsAttr):
		return store.RoleDrafts
	case hasAttr(imap.TrashAttr):
		return store.RoleTrash
	case hasAttr(imap.JunkAttr):
		return store.RoleSpam
	case hasAttr(imap.ArchiveAttr):
		return store.RoleArchive
	}

	name := strings.ToLower(strings.TrimSpace(info.Name))
	trimmed := strings.Trim(name, "/")
	switch {
	case trimmed == "inbox" || strings.HasSuffix(trimmed, "/inbox"):
		return store.RoleInbox
	case containsAny(trimmed, "已发送", "sent", "sent items", "sent messages", "sent mail"):
		return store.RoleSent
	case containsAny(trimmed, "草稿", "draft"):
		return store.RoleDrafts
	case containsAny(trimmed, "已删除", "垃圾箱", "trash", "deleted", "deleted items", "bin"):
		return store.RoleTrash
	case containsAny(trimmed, "垃圾邮件", "spam", "junk"):
		return store.RoleSpam
	case containsAny(trimmed, "归档", "archive"):
		return store.RoleArchive
	}
	return store.RoleCustom
}

func containsAny(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// displayFolderName 去掉命名空间前缀，得到用于展示的文件夹名。
func displayFolderName(remote string) string {
	name := strings.TrimSpace(remote)
	if name == "" {
		return remote
	}
	if idx := strings.LastIndexAny(name, "/."); idx >= 0 && idx < len(name)-1 {
		return name[idx+1:]
	}
	return name
}
