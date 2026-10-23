// Package realtime 实现进程内 SSE 事件广播。
package realtime

import (
	"sync"
	"sync/atomic"
)

// 事件名。
const (
	EventMailNew    = "mail:new"
	EventMailCode   = "mail:code"
	EventMailUpdate = "mail:update"
	EventSyncStatus = "sync:status"
)

// Event 是一条待广播事件。
type Event struct {
	Name string
	Data any
}

// MailNew 对应 mail:new 事件。
type MailNew struct {
	ID           string `json:"id"`
	AccountID    string `json:"accountId"`
	AccountEmail string `json:"accountEmail"`
	FolderID     string `json:"folderId"`
	Subject      string `json:"subject"`
	FromName     string `json:"fromName"`
	FromEmail    string `json:"fromEmail"`
	Snippet      string `json:"snippet"`
	ReceivedAt   string `json:"receivedAt"`
	IsRead       bool   `json:"isRead"`
	IsStarred    bool   `json:"isStarred"`
	Code         string `json:"code,omitempty"`
}

// MailCode 对应 mail:code 事件。
type MailCode struct {
	MessageID string `json:"messageId"`
	Code      string `json:"code"`
	From      string `json:"from"`
	Subject   string `json:"subject"`
}

// SyncStatus 对应 sync:status 事件。
type SyncStatus struct {
	AccountID string `json:"accountId"`
	Status    string `json:"status"`
	Message   string `json:"message,omitempty"`
}

// MailUpdate 对应 mail:update 事件（已读/星标/删除等本地状态变化）。
type MailUpdate struct {
	ID        string `json:"id"`
	AccountID string `json:"accountId"`
	FolderID  string `json:"folderId"`
	IsRead    *bool  `json:"isRead,omitempty"`
	IsStarred *bool  `json:"isStarred,omitempty"`
	Deleted   bool   `json:"deleted,omitempty"`
}

// Hub 是无阻塞广播中心：慢订阅者被丢弃，不阻塞发布者。
type Hub struct {
	mu      sync.RWMutex
	subs    map[int64]chan Event
	nextID  atomic.Int64
	bufSize int
	dropped atomic.Int64
}

// NewHub 创建 Hub。
func NewHub() *Hub {
	return &Hub{subs: make(map[int64]chan Event), bufSize: 32}
}

// Subscribe 注册一个订阅者，返回事件通道与取消函数。
func (h *Hub) Subscribe() (<-chan Event, func()) {
	id := h.nextID.Add(1)
	ch := make(chan Event, h.bufSize)
	h.mu.Lock()
	h.subs[id] = ch
	h.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			h.mu.Lock()
			if c, ok := h.subs[id]; ok {
				delete(h.subs, id)
				close(c)
			}
			h.mu.Unlock()
		})
	}
	return ch, cancel
}

// Publish 向所有订阅者投递事件；缓冲已满的订阅者会丢弃该事件。
func (h *Hub) Publish(name string, data any) {
	ev := Event{Name: name, Data: data}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, ch := range h.subs {
		select {
		case ch <- ev:
		default:
			h.dropped.Add(1)
		}
	}
}

// Subscribers 返回当前订阅者数量。
func (h *Hub) Subscribers() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs)
}

// Dropped 返回累计丢弃事件数。
func (h *Hub) Dropped() int64 { return h.dropped.Load() }
