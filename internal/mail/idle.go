package mail

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"github.com/emersion/go-imap"
	imapclient "github.com/emersion/go-imap/client"

	"zmail/internal/store"
)

// idleWatcher 维护账号的长连接：连接 -> 登录 -> SELECT INBOX -> CATCHUP -> IDLE，
// 断线后指数退避重连（1s→2s→4s…max 60s + 抖动）。认证失败则标记 auth_failed 并停止重试。
func (m *Manager) idleWatcher(ctx context.Context, rt *accountRuntime, acc *store.Account) {
	delay := reconnectMinDelay
	for {
		select {
		case <-ctx.Done():
			rt.setState(StateDisconnected)
			return
		default:
		}

		err := m.runIDLE(ctx, rt, acc)
		if ctx.Err() != nil {
			rt.setState(StateDisconnected)
			return
		}
		if err == nil {
			// 连接被正常重建（例如收到刷新信号），无需退避；短暂让出避免空转。
			delay = reconnectMinDelay
			if !sleepCtx(ctx, 500*time.Millisecond) {
				rt.setState(StateDisconnected)
				return
			}
			continue
		}
		if isAuthError(err) {
			m.markAuthFailed(ctx, acc, err)
			return
		}

		m.log.Warn("IMAP 实时连接断开，准备重连",
			"account", acc.ID, "error", err, "delay", delay.String())
		rt.setError(StateError, "与 IMAP 服务器的连接已断开")
		m.publishSync(acc.ID, "error", "连接断开，正在重连")
		if !sleepCtx(ctx, withJitter(delay)) {
			rt.setState(StateDisconnected)
			return
		}
		delay *= 2
		if delay > reconnectMaxDelay {
			delay = reconnectMaxDelay
		}
	}
}

// runIDLE 执行一次完整的 IDLE 会话，直到上下文取消或连接异常。
func (m *Manager) runIDLE(ctx context.Context, rt *accountRuntime, acc *store.Account) error {
	rt.setState(StateConnecting)
	rt.setIDLE(false)

	conn, err := m.openIMAP(ctx, acc, imapDialTimeout)
	if err != nil {
		m.publishSync(acc.ID, "error", "连接 IMAP 服务器失败")
		return err
	}
	defer conn.close()

	usingIDLE := conn.hasCap("IDLE")
	rt.setIDLE(usingIDLE)
	rt.setState(StateAuth)

	// IDLE 期间给命令留一个宽松上限，避免网络静默导致永久挂起。
	conn.client.Timeout = m.idleRefresh() + 2*time.Minute

	rt.setState(StateSelect)
	mb, err := conn.client.Select(imap.InboxName, false)
	if err != nil {
		return fmt.Errorf("mail: 打开收件箱失败: %w", err)
	}

	// CATCHUP：交给同步 worker 用独立连接补拉 lastUID+1:*。
	rt.setState(StateCatchup)
	m.notifySync(acc.ID)

	rt.setState(StateIdle)
	if usingIDLE {
		m.publishSync(acc.ID, "idle", "")
	} else {
		m.log.Info("IMAP 服务器不支持 IDLE，降级为轮询", "account", acc.ID)
		m.publishSync(acc.ID, "idle", "服务器不支持 IDLE，已降级为轮询")
	}

	updates := make(chan imapclient.Update, 32)
	conn.client.Updates = updates
	stop := make(chan struct{})
	idleErr := make(chan error, 1)
	opts := &imapclient.IdleOptions{
		LogoutTimeout: m.idleRefresh(),
		PollInterval:  idlePollInterval,
	}
	go func() { idleErr <- conn.client.Idle(stop, opts) }()

	lastSeen := mb.Messages
	for {
		select {
		case <-ctx.Done():
			close(stop)
			// 强制中断读等待，保证 Stop 能立刻返回。
			conn.forceClose()
			select {
			case <-idleErr:
			case <-time.After(2 * time.Second):
			}
			return nil

		case err := <-idleErr:
			if err == nil {
				return nil
			}
			return fmt.Errorf("mail: IDLE 结束: %w", err)

		case u, ok := <-updates:
			if !ok {
				return nil
			}
			switch up := u.(type) {
			case *imapclient.MailboxUpdate:
				if up.Mailbox == nil {
					continue
				}
				if up.Mailbox.Messages > lastSeen {
					lastSeen = up.Mailbox.Messages
					rt.touch()
					m.notifySync(acc.ID)
					m.publishSync(acc.ID, "syncing", "收到新邮件通知")
				}
			case *imapclient.StatusUpdate:
				if up.Status != nil && up.Status.Type == imap.StatusRespBye {
					return fmt.Errorf("mail: 服务器结束连接: %s", up.Status.Info)
				}
			}
		}
	}
}

// idleRefresh 返回 IDLE 重发间隔（默认 25 分钟）。
func (m *Manager) idleRefresh() time.Duration {
	if m.cfg != nil && m.cfg.IdleRefresh > 0 {
		return m.cfg.IdleRefresh
	}
	return 1500 * time.Second
}

// sleepCtx 在上下文取消时可提前返回。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// withJitter 给退避时间加 ±20% 抖动，避免多账号同时重连。
func withJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	delta := int64(d) / 5
	if delta <= 0 {
		return d
	}
	return d + time.Duration(rand.Int63n(2*delta+1)-delta)
}
