# zMail 内部接口契约

> 本文记录当前包依赖方向、核心接口与前端 DOM 契约，供维护和二次开发参考；具体签名以代码为准。

## 包依赖方向

```
config   ← 无依赖
secret   ← 无依赖
verify   ← 无依赖
realtime ← 无依赖
store    ← 只依赖标准库 + modernc.org/sqlite
mail     → config, secret, store, realtime, verify
httpapi  → config, secret, store, mail, realtime, verify, web
web      → 无依赖（仅 embed 静态资源）
cmd/zmail→ 全部
```

## internal/store（Lead 已冻结：schema.sql / model.go / store.go / blobs.go）

已存在：

```go
func Open(dbPath, rawDir string) (*Store, error)
func (s *Store) Migrate(ctx context.Context) error
func (s *Store) Close() error
func (s *Store) DB() *sql.DB
func (s *Store) Blobs() *Blobs
func NewID() string
func (s *Store) Ping(ctx context.Context) error
func (s *Store) Stats(ctx context.Context) (Stats, error)
func (s *Store) CleanupRuntimeData(ctx context.Context, logRetention, jobRetention time.Duration) (int64, int64, error)
```

需要实现的方法（签名冻结，实现放在 account.go/folder.go/message.go/search.go/jobs.go/logs.go/settings.go）：

```go
// account.go
func (s *Store) CreateAccount(ctx context.Context, a *Account) error
func (s *Store) UpdateAccount(ctx context.Context, a *Account) error
func (s *Store) GetAccount(ctx context.Context, id string) (*Account, error)
func (s *Store) GetAccountByEmail(ctx context.Context, email string) (*Account, error)
func (s *Store) ListAccounts(ctx context.Context) ([]*Account, error)
func (s *Store) DeleteAccount(ctx context.Context, id string) error
func (s *Store) SetAccountStatus(ctx context.Context, id, status, lastError string) error
func (s *Store) TouchAccount(ctx context.Context, id string) error

// folder.go
func (s *Store) UpsertFolder(ctx context.Context, f *Folder) error            // 冲突键 (account_id, remote_path)；f.ID 为空时生成
func (s *Store) GetFolder(ctx context.Context, id string) (*Folder, error)
func (s *Store) GetFolderByRemotePath(ctx context.Context, accountID, remotePath string) (*Folder, error)
func (s *Store) GetFolderByRole(ctx context.Context, accountID, role string) (*Folder, error)
func (s *Store) ListFolders(ctx context.Context, accountID string) ([]*Folder, error) // accountID=="" 返回全部
func (s *Store) UpdateFolderCursor(ctx context.Context, folderID string, uidValidity, lastUID uint32) error
func (s *Store) UpdateFolderCounts(ctx context.Context, folderID string, unread, total int) error
func (s *Store) ResetFolderMessages(ctx context.Context, folderID string, uidValidity uint32) error
func (s *Store) SetFolderEnabled(ctx context.Context, folderID string, enabled bool) error
func (s *Store) DeleteFolder(ctx context.Context, folderID string) error

// message.go
func (s *Store) InsertMessages(ctx context.Context, bundles []*MessageBundle) (int, error) // 单事务，去重，返回实际插入数
func (s *Store) GetMessage(ctx context.Context, id string) (*Message, []*Attachment, error)
func (s *Store) ListMessages(ctx context.Context, f ListFilter) ([]*MessageSummary, string, error) // 第二返回值为 nextCursor
func (s *Store) SetMessageFlags(ctx context.Context, id string, isRead, isStarred *bool) (*Message, error)
func (s *Store) SoftDeleteMessage(ctx context.Context, id string) error
func (s *Store) HardDeleteMessage(ctx context.Context, id string) error
func (s *Store) MessageExists(ctx context.Context, accountID, messageID string) (bool, error)
func (s *Store) UnreadCounts(ctx context.Context) (map[string]int, error) // key: folder_id
func (s *Store) UnreadTotal(ctx context.Context) (int, error)
func (s *Store) RecentMessages(ctx context.Context, accountID string, limit int) ([]*MessageSummary, error)

// search.go
func (s *Store) Search(ctx context.Context, query, accountID string, limit int) ([]*MessageSummary, error)

// jobs.go
func (s *Store) EnqueueJob(ctx context.Context, j *Job) error
func (s *Store) DueJobs(ctx context.Context, limit int) ([]*Job, error)
func (s *Store) ClaimJob(ctx context.Context, id string) error
func (s *Store) FinishJob(ctx context.Context, id string, err error) error // 失败时 attempts+1，>=3 置 failed，否则延后重试

// logs.go
func (s *Store) AddSyncLog(ctx context.Context, accountID, level, message string) error
func (s *Store) ListSyncLogs(ctx context.Context, accountID string, limit int) ([]*SyncLog, error)

// settings.go
func (s *Store) GetSetting(ctx context.Context, key string) (string, bool, error)
func (s *Store) SetSetting(ctx context.Context, key, value string) error
func (s *Store) ListSettings(ctx context.Context) (map[string]string, error)
```

## internal/mail（冻结）

```go
type Manager struct{ /* 不导出字段 */ }
func NewManager(cfg *config.Config, st *store.Store, box *secret.Box, hub *realtime.Hub, log *slog.Logger) *Manager

func (m *Manager) Start(ctx context.Context)
func (m *Manager) Stop()
func (m *Manager) ReloadAccount(ctx context.Context, accountID string)
func (m *Manager) RemoveAccount(accountID string)
func (m *Manager) SyncAccount(ctx context.Context, accountID string) error
func (m *Manager) SyncFolder(ctx context.Context, folderID string) (int, error)
func (m *Manager) DiscoverFolders(ctx context.Context, accountID string) ([]*store.Folder, error)
func (m *Manager) TestConnection(ctx context.Context, acc *store.Account) error
func (m *Manager) TestAccount(ctx context.Context, accountID string) error
func (m *Manager) Send(ctx context.Context, req SendRequest) (*store.Message, error)
func (m *Manager) SendQueued(ctx context.Context, job *store.Job) error
func (m *Manager) Statuses() []AccountStatus
func (m *Manager) Status(accountID string) AccountStatus

type SendRequest struct {
	AccountID  string
	To, Cc, Bcc []string
	Subject    string
	TextBody   string
	HTMLBody   string
	InReplyTo  string
	References []string
	Attachments []Attachment
}
type Attachment struct { Filename, MimeType string; Data []byte }

type AccountStatus struct {
	AccountID   string    `json:"accountId"`
	State       string    `json:"state"`
	UsingIDLE   bool      `json:"usingIdle"`
	LastError   string    `json:"lastError,omitempty"`
	LastEventAt time.Time `json:"lastEventAt"`
}

// provider.go
type Provider struct { Key, Name, IMAPHost string; IMAPPort int; SMTPHost string; SMTPPort int; SMTPSecure bool; Notes string }
func Providers() []Provider
func LookupProvider(key string) (Provider, bool)

// parse.go
type ParsedMessage struct {
	MessageID  string
	InReplyTo  string
	References []string
	Subject    string
	FromName   string
	FromEmail  string
	To, Cc, Bcc []store.Addr
	Date       time.Time
	TextBody   string
	HTMLBody   string
	Snippet    string
	Attachments []*store.AttachmentPayload
	Raw        []byte
	Size       int64
}
func Parse(r io.Reader) (*ParsedMessage, error)
func ParseBytes(data []byte) (*ParsedMessage, error)
```

## internal/httpapi（冻结）

```go
type Deps struct {
	Cfg   *config.Config
	Store *store.Store
	Hub   *realtime.Hub
	Mail  *mail.Manager
	Log   *slog.Logger
}
func New(d Deps) (*Server, error)
func (s *Server) Handler() http.Handler
```

路由（见 docs/BUILD.md 第 8 章）：`/healthz` `/login` `/logout` `/mail` `/settings/accounts` `/settings/general`
以及 `/api/accounts`、`/api/folders`、`/api/messages`、`/api/messages/{id}`、`/api/messages/send`、`/api/attachments/{id}`、`/api/search`、`/api/events`。

## internal/web（冻结）

```go
package web
//go:embed static
var assets embed.FS
func Static() fs.FS
```

静态资源目录 `internal/web/static/`：app.css、app.js、htmx.min.js、manifest.webmanifest、sw.js、icons/。

## 前端 DOM 契约（httpapi 模板 ↔ app.js）

- 布局：`.topbar`（#menuBtn .brand #syncState #themeBtn #helpBtn）、`.layout`（`.sidebar`/`.list-pane`/`.reader`）
- 列表：#messageList（.message-item[data-id][data-account-id]，.unread/.starred，.subject/.from/.snippet/.time）
- 阅读器：#reader（.reader-head .reader-body .reader-actions，#codeBox）
- 搜索：#search；命令面板：#palette/#paletteInput/#paletteResults
- Toast：#toast；帮助：#help
- 侧栏：#sidebarContent（[data-folder-id][data-account-id]，.account-group .folder-item .badge）
- 数据注入：`window.__ZMAIL__ = {accountId, folderId, messageId, csrfToken, baseUrl}`
- JS 通过 `fetch('/api/...', {credentials:'same-origin'})` 调用接口；SSE 订阅 `/api/events`。
