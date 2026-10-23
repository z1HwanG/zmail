package httpapi

import (
	"context"
	"net/http"
	"strings"
	"time"

	"zmail/internal/store"
)

// backfillRequest 是手动拉取更早邮件的请求体。
type backfillRequest struct {
	FolderID  string `json:"folderId"`
	Scope     string `json:"scope"`
	AccountID string `json:"accountId"`
	Limit     int    `json:"limit"`
}

// handleBackfill 手动触发历史回填：一次请求拉取一批更早的邮件。
//   - 指定 folderId：只回填该文件夹；
//   - scope=inbox（默认）：回填所有账号的收件箱，每次最多 3 个文件夹。
func (s *Server) handleBackfill(w http.ResponseWriter, r *http.Request) {
	if s.mail == nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "邮件引擎未就绪")
		return
	}
	var req backfillRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "请求体格式错误: "+err.Error())
		return
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 150*time.Second)
	defer cancel()

	if strings.TrimSpace(req.FolderID) != "" {
		folder, err := s.st.GetFolder(ctx, req.FolderID)
		if err != nil {
			s.storeError(w, r, "文件夹不存在", err)
			return
		}
		inserted, err := s.mail.SyncFolderOlder(ctx, folder.ID, limit)
		if err != nil {
			s.log.Warn("手动历史回填失败", "folder_id", folder.ID, "err", err)
			writeError(w, r, http.StatusBadGateway, codeUpstreamError, "拉取更早邮件失败: "+err.Error())
			return
		}
		fresh, _ := s.st.GetFolder(ctx, folder.ID)
		done := fresh != nil && fresh.BackfillUID < 0
		writeJSON(w, http.StatusOK, map[string]any{
			"inserted": inserted, "folders": 1, "done": done,
		})
		return
	}

	// 聚合收件箱：按“未回填完优先、最久未更新优先”挑选最多 3 个收件箱。
	folders, err := s.st.ListFolders(ctx, strings.TrimSpace(req.AccountID))
	if err != nil {
		s.internalError(w, r, "查询文件夹失败", err)
		return
	}
	candidates := make([]*store.Folder, 0, len(folders))
	for _, f := range folders {
		if f == nil || !f.Enabled || f.Role != store.RoleInbox || f.BackfillUID < 0 {
			continue
		}
		candidates = append(candidates, f)
	}
	if len(candidates) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"inserted": 0, "folders": 0, "done": true})
		return
	}
	sortFoldersForBackfill(candidates)
	if len(candidates) > 3 {
		candidates = candidates[:3]
	}

	inserted, used := 0, 0
	var firstErr error
	for _, f := range candidates {
		if err := ctx.Err(); err != nil {
			break
		}
		n, berr := s.mail.SyncFolderOlder(ctx, f.ID, limit)
		if berr != nil {
			if firstErr == nil {
				firstErr = berr
			}
			s.log.Warn("手动历史回填失败", "folder_id", f.ID, "err", berr)
			continue
		}
		inserted += n
		used++
	}
	if used == 0 && firstErr != nil {
		writeError(w, r, http.StatusBadGateway, codeUpstreamError, "拉取更早邮件失败: "+firstErr.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"inserted": inserted, "folders": used, "done": false})
}

// sortFoldersForBackfill 让“从未回填”的文件夹优先，其余按上次同步时间升序。
func sortFoldersForBackfill(list []*store.Folder) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0; j-- {
			a, b := list[j-1], list[j]
			swap := false
			aNew, bNew := a.BackfillUID == 0, b.BackfillUID == 0
			if aNew != bNew {
				swap = bNew
			} else {
				at, bt := time.Time{}, time.Time{}
				if a.LastSyncedAt != nil {
					at = *a.LastSyncedAt
				}
				if b.LastSyncedAt != nil {
					bt = *b.LastSyncedAt
				}
				swap = bt.Before(at)
			}
			if !swap {
				break
			}
			list[j-1], list[j] = list[j], list[j-1]
		}
	}
}
