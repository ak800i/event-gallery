package httpapi

import (
	"crypto/subtle"
	"net/http"
	"strings"
	"time"

	"wedding-gallery/backend/internal/models"
	"wedding-gallery/backend/internal/store"
)

type loginRequest struct {
	Password string `json:"password"`
}

// handleAdminLogin authenticates against the single ADMIN_PASSWORD shared
// secret (there is no admin username by design). A dedicated, much
// stricter per-IP rate limit guards against brute-force password guessing
// on top of the general public API limiter.
func (s *Server) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !s.loginLimiter.Allow(ip) {
		writeError(w, http.StatusTooManyRequests, "too many login attempts, please wait before trying again")
		return
	}

	var req loginRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	match := subtle.ConstantTimeCompare([]byte(req.Password), []byte(s.cfg.AdminPassword)) == 1
	if !match {
		_ = s.store.RecordAudit(r.Context(), models.ActionLoginFailed, ip, "", "", "")
		writeError(w, http.StatusUnauthorized, "invalid password")
		return
	}

	sess, err := s.store.CreateSession(r.Context(), s.cfg.SessionTTL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create session")
		return
	}
	setSessionCookies(w, sess, s.cfg.CookieSecure)
	_ = s.store.RecordAudit(r.Context(), models.ActionLogin, ip, "", "", "")
	writeJSON(w, http.StatusOK, map[string]string{"csrfToken": sess.CSRFToken})
}

type adminSessionResponse struct {
	Authenticated bool   `json:"authenticated"`
	CSRFToken     string `json:"csrfToken"`
}

func (s *Server) handleAdminSession(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	writeJSON(w, http.StatusOK, adminSessionResponse{Authenticated: true, CSRFToken: sess.CSRFToken})
}

func (s *Server) handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess != nil {
		_ = s.store.DeleteSession(r.Context(), sess.ID)
	}
	clearSessionCookies(w, s.cfg.CookieSecure)
	_ = s.store.RecordAudit(r.Context(), models.ActionLogout, clientIP(r), "", "", "")
	writeJSON(w, http.StatusOK, map[string]string{"status": "logged out"})
}

// handleAdminListMedia returns a page of all media items for the admin
// dashboard, optionally filtered by status (active/trashed); omitting the
// filter returns every item regardless of status.
func (s *Server) handleAdminListMedia(w http.ResponseWriter, r *http.Request) {
	statusParam := models.MediaStatus(r.URL.Query().Get("status"))
	if statusParam != models.StatusActive && statusParam != models.StatusTrashed {
		statusParam = ""
	}
	cursor, err := store.DecodeCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid cursor")
		return
	}
	limit := parseLimit(r, 50, 200)

	items, nextCursor, err := s.store.ListAdmin(r.Context(), store.AdminListParams{
		Status: statusParam, Cursor: cursor, Limit: limit,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list media")
		return
	}
	dtos := make([]mediaItemDTO, 0, len(items))
	for _, item := range items {
		dtos = append(dtos, toDTO(item, true))
	}
	writeJSON(w, http.StatusOK, galleryResponse{Items: dtos, NextCursor: nextCursor})
}

type bulkIDsRequest struct {
	IDs []string `json:"ids"`
}

func (s *Server) bulkChangeStatus(w http.ResponseWriter, r *http.Request, target models.MediaStatus, action models.AuditAction) {
	var req bulkIDsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.IDs) == 0 {
		writeError(w, http.StatusBadRequest, "no ids provided")
		return
	}
	if len(req.IDs) > 500 {
		writeError(w, http.StatusBadRequest, "too many ids in a single request (max 500)")
		return
	}

	ctx := r.Context()
	// Look up filenames before the status change for accurate audit
	// entries; missing IDs are silently ignored.
	filenames := make(map[string]string, len(req.IDs))
	for _, id := range req.IDs {
		if item, err := s.store.GetByID(ctx, id, ""); err == nil {
			filenames[id] = item.OriginalFilename
		}
	}

	changed, err := s.store.SetStatusBulk(ctx, req.IDs, target)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update media status")
		return
	}

	actor := "admin"
	for _, id := range changed {
		_ = s.store.RecordAudit(ctx, action, actor, id, filenames[id], "")
	}

	writeJSON(w, http.StatusOK, map[string]any{"changed": changed})
}

func (s *Server) handleBulkDelete(w http.ResponseWriter, r *http.Request) {
	s.bulkChangeStatus(w, r, models.StatusTrashed, models.ActionDelete)
}

func (s *Server) handleBulkRestore(w http.ResponseWriter, r *http.Request) {
	s.bulkChangeStatus(w, r, models.StatusActive, models.ActionRestore)
}

type auditEntryDTO struct {
	ID        int64  `json:"id"`
	Action    string `json:"action"`
	Actor     string `json:"actor"`
	MediaID   string `json:"mediaId,omitempty"`
	Filename  string `json:"filename,omitempty"`
	Details   string `json:"details,omitempty"`
	CreatedAt string `json:"createdAt"`
}

type auditLogResponse struct {
	Entries    []auditEntryDTO `json:"entries"`
	NextCursor string          `json:"nextCursor,omitempty"`
}

func (s *Server) handleAuditLog(w http.ResponseWriter, r *http.Request) {
	cursor, err := store.DecodeCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid cursor")
		return
	}
	limit := parseLimit(r, 50, 200)

	entries, nextCursor, err := s.store.ListAudit(r.Context(), store.ListAuditParams{Cursor: cursor, Limit: limit})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load audit log")
		return
	}
	dtos := make([]auditEntryDTO, 0, len(entries))
	for _, e := range entries {
		dtos = append(dtos, auditEntryDTO{
			ID: e.ID, Action: string(e.Action), Actor: e.Actor, MediaID: e.MediaID,
			Filename: e.Filename, Details: e.Details, CreatedAt: e.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, auditLogResponse{Entries: dtos, NextCursor: nextCursor})
}

type adminConfigResponse struct {
	UploadExpiresAt *string `json:"uploadExpiresAt,omitempty"`
}

func (s *Server) handleAdminGetConfig(w http.ResponseWriter, r *http.Request) {
	resp := adminConfigResponse{}
	if value, ok, err := s.store.GetConfig(r.Context(), store.ConfigKeyUploadExpiresAt); err == nil && ok && value != "" {
		resp.UploadExpiresAt = &value
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load configuration")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

type adminConfigUpdateRequest struct {
	// UploadExpiresAt is an RFC3339 timestamp after which new uploads are
	// refused, or null/empty to clear the expiry (uploads always allowed).
	// Editing this NEVER affects the ability to view or download existing
	// media -- only whether new uploads are accepted.
	UploadExpiresAt *string `json:"uploadExpiresAt"`
}

func (s *Server) handleAdminUpdateConfig(w http.ResponseWriter, r *http.Request) {
	var req adminConfigUpdateRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	ctx := r.Context()
	details := "upload expiry cleared"
	if req.UploadExpiresAt == nil || strings.TrimSpace(*req.UploadExpiresAt) == "" {
		if err := s.store.DeleteConfig(ctx, store.ConfigKeyUploadExpiresAt); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to update configuration")
			return
		}
	} else {
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(*req.UploadExpiresAt))
		if err != nil {
			writeError(w, http.StatusBadRequest, "uploadExpiresAt must be an RFC3339 timestamp")
			return
		}
		normalized := parsed.UTC().Format(time.RFC3339Nano)
		if err := s.store.SetConfig(ctx, store.ConfigKeyUploadExpiresAt, normalized); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to update configuration")
			return
		}
		details = "upload expiry set to " + normalized
	}

	_ = s.store.RecordAudit(ctx, models.ActionConfig, "admin", "", "", details)
	s.handleAdminGetConfig(w, r)
}
