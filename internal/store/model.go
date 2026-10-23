package store

import (
	"encoding/json"
	"fmt"
	"time"
)

// TimeLayout 是数据库中所有时间字段的统一格式（UTC，毫秒定长，保证字典序等于时间序）。
const TimeLayout = "2006-01-02T15:04:05.000Z07:00"

// 账号状态。
const (
	AccountActive     = "active"
	AccountAuthFailed = "auth_failed"
	AccountDisabled   = "disabled"
)

// 文件夹角色。
const (
	RoleInbox   = "inbox"
	RoleSent    = "sent"
	RoleDrafts  = "drafts"
	RoleTrash   = "trash"
	RoleSpam    = "spam"
	RoleArchive = "archive"
	RoleCustom  = "custom"
)

// 任务状态。
const (
	JobPending = "pending"
	JobRunning = "running"
	JobDone    = "done"
	JobFailed  = "failed"
)

// Addr 表示一个邮件地址。
type Addr struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// Account 是一个邮箱账号。PasswordEnc 为 AES-256-GCM 密文。
type Account struct {
	ID           string
	Provider     string
	Email        string
	DisplayName  string
	Username     string
	PasswordEnc  []byte
	IMAPHost     string
	IMAPPort     int
	IMAPSecure   bool
	SMTPHost     string
	SMTPPort     int
	SMTPSecure   bool
	Status       string
	LastError    string
	LastSyncedAt *time.Time
	CreatedAt    time.Time
}

// Folder 是账号下的一个 IMAP 文件夹。
type Folder struct {
	ID           string
	AccountID    string
	RemotePath   string
	Name         string
	Role         string
	UIDValidity  uint32
	LastUID      uint32
	UnreadCount  int
	TotalCount   int
	Enabled      bool
	Priority     int
	LastSyncedAt *time.Time
	// BackfillUID 是历史回填游标：0 表示尚未开始，-1 表示已回填完更早的邮件，
	// 大于 0 表示下一次回填从这个 UID（不含）向下继续。
	BackfillUID int
}

// Message 是一封已入库的邮件。
type Message struct {
	ID             string
	AccountID      string
	FolderID       string
	ThreadID       string
	RemoteUID      uint32
	MessageID      string
	Subject        string
	FromName       string
	FromEmail      string
	To             []Addr
	Cc             []Addr
	Bcc            []Addr
	SentAt         *time.Time
	ReceivedAt     time.Time
	Snippet        string
	BodyText       string
	BodyHTML       string
	HasAttachments bool
	IsRead         bool
	IsStarred      bool
	IsDeleted      bool
	IsDraft        bool
	SizeBytes      int64
	RawKey         string
	CreatedAt      time.Time

	// 以下字段来自 JOIN，仅用于展示，不写入 messages 表。
	AccountEmail string
	AccountName  string
	FolderName   string
}

// Attachment 是邮件附件元数据；内容以 sha256 为键存放在 blob 目录。
type Attachment struct {
	ID         string
	MessageID  string
	Filename   string
	MimeType   string
	Size       int64
	ContentID  string
	SHA256     string
	StorageKey string
}

// AttachmentPayload 是解析阶段得到的附件内容。
type AttachmentPayload struct {
	Filename  string
	MimeType  string
	ContentID string
	Data      []byte
}

// MessageBundle 是插入一封邮件所需的全部数据。
type MessageBundle struct {
	Message     *Message
	Attachments []*AttachmentPayload
	Raw         []byte
}

// MessageSummary 是列表接口使用的精简结构。
type MessageSummary struct {
	ID             string    `json:"id"`
	AccountID      string    `json:"accountId"`
	AccountEmail   string    `json:"accountEmail"`
	AccountName    string    `json:"accountName"`
	FolderID       string    `json:"folderId"`
	FolderName     string    `json:"folderName"`
	Subject        string    `json:"subject"`
	FromName       string    `json:"fromName"`
	FromEmail      string    `json:"fromEmail"`
	Snippet        string    `json:"snippet"`
	ReceivedAt     time.Time `json:"receivedAt"`
	IsRead         bool      `json:"isRead"`
	IsStarred      bool      `json:"isStarred"`
	HasAttachments bool      `json:"hasAttachments"`
}

// Thread 是邮件会话。
type Thread struct {
	ID            string
	AccountID     string
	ThreadKey     string
	Subject       string
	LastMessageAt time.Time
	MessageCount  int
}

// Job 是持久化任务（回填、重试发送等）。
type Job struct {
	ID        string
	Kind      string
	AccountID string
	Payload   string
	RunAt     time.Time
	Attempts  int
	Status    string
	LastError string
	CreatedAt time.Time
}

// SyncLog 是同步日志。
type SyncLog struct {
	ID        string
	AccountID string
	Level     string
	Message   string
	CreatedAt time.Time
}

// ListFilter 描述邮件列表查询。
type ListFilter struct {
	AccountID   string
	FolderID    string
	Scope       string // inbox = 所有账号的收件箱聚合
	Before      time.Time
	Cursor      string
	Limit       int
	UnreadOnly  bool
	StarredOnly bool
}

// Stats 是给 /healthz 与设置页用的统计。
type Stats struct {
	Accounts  int
	Folders   int
	Messages  int
	Unread    int
	Starred   int
	SizeBytes int64
}

func formatTime(t time.Time) string { return t.UTC().Format(TimeLayout) }

func formatTimePtr(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return formatTime(*t)
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(TimeLayout, s); err == nil {
		return t.UTC()
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func parseTimePtr(s *string) *time.Time {
	if s == nil || *s == "" {
		return nil
	}
	t := parseTime(*s)
	if t.IsZero() {
		return nil
	}
	return &t
}

func marshalAddrs(addrs []Addr) string {
	if len(addrs) == 0 {
		return "[]"
	}
	b, err := json.Marshal(addrs)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func unmarshalAddrs(s string) []Addr {
	if s == "" {
		return nil
	}
	var out []Addr
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Validate 检查账号必填字段。
func (a *Account) Validate() error {
	switch {
	case a.Email == "":
		return fmt.Errorf("%w: 邮箱地址不能为空", ErrInvalid)
	case a.Username == "":
		return fmt.Errorf("%w: 用户名不能为空", ErrInvalid)
	case a.IMAPHost == "":
		return fmt.Errorf("%w: IMAP 主机不能为空", ErrInvalid)
	case a.SMTPHost == "":
		return fmt.Errorf("%w: SMTP 主机不能为空", ErrInvalid)
	case len(a.PasswordEnc) == 0:
		return fmt.Errorf("%w: 密码不能为空", ErrInvalid)
	}
	return nil
}
