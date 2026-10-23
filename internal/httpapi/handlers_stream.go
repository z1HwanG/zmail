package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// handleEvents 提供 SSE 事件流：立即发送首帧，之后 20 秒一次心跳。
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "服务器不支持流式响应")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("X-Accel-Buffering", "no")
	h.Set("Connection", "keep-alive")
	h.Del("Server-Timing")
	w.WriteHeader(http.StatusOK)
	// 首帧立即 flush，让客户端（含反代）尽快建立流。
	fmt.Fprint(w, "retry: 3000\n\n: connected\n\n")
	flusher.Flush()

	events, cancel := s.hub.Subscribe()
	defer cancel()
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			data, err := json.Marshal(ev.Data)
			if err != nil {
				s.log.Warn("SSE 事件序列化失败", "event", ev.Name, "err", err)
				continue
			}
			name := strings.ReplaceAll(strings.ReplaceAll(ev.Name, "\n", ""), "\r", "")
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
