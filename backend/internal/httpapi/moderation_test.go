package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"event-gallery/backend/internal/models"
	"event-gallery/backend/internal/store"
)

func TestModeration_DefaultOffAndApprovalFlow(t *testing.T) {
	h := newTestHarness(t)

	pubRec := doRequest(h, http.MethodGet, "/api/config/public", nil)
	var pubConfig publicConfigResponse
	_ = json.Unmarshal(pubRec.Body.Bytes(), &pubConfig)
	if pubConfig.ApprovalRequired {
		t.Fatal("approval must default off")
	}

	sess, csrfCookie, token := adminLogin(t, h, h.cfg.AdminPassword)
	enableReq := authedRequest(h, http.MethodPut, "/api/admin/moderation", []byte(`{"approvalRequired":true}`), sess, csrfCookie, token)
	enableRec := serveRequest(h, enableReq)
	if enableRec.Code != http.StatusOK {
		t.Fatalf("enable moderation: %d %s", enableRec.Code, enableRec.Body.String())
	}

	pendingSHA := repeatChar('a', 64)
	insertTestMedia(t, h, "pending-id", pendingSHA, time.Now())
	galleryRec := doRequest(h, http.MethodGet, "/api/gallery", nil)
	var gallery galleryResponse
	_ = json.Unmarshal(galleryRec.Body.Bytes(), &gallery)
	if len(gallery.Items) != 0 {
		t.Fatalf("pending media leaked into gallery: %+v", gallery.Items)
	}
	for _, path := range []string{
		"/api/media/pending-id/thumbnail",
		"/api/media/pending-id/file",
		"/api/media/pending-id/download",
	} {
		if rec := doRequest(h, http.MethodGet, path, nil); rec.Code != http.StatusNotFound {
			t.Fatalf("pending public path %s should be 404, got %d", path, rec.Code)
		}
	}
	checkBody := []byte(`{"sha256":"` + pendingSHA + `","size":100,"filename":"photo.jpg"}`)
	checkRec := doRequest(h, http.MethodPost, "/api/uploads/check", checkBody)
	var duplicate uploadCheckResponse
	_ = json.Unmarshal(checkRec.Body.Bytes(), &duplicate)
	if !duplicate.Duplicate || duplicate.MediaID != "" {
		t.Fatalf("pending duplicate must not disclose media ID: %+v", duplicate)
	}
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		likeReq := newRequestWithHeader(method, "/api/media/pending-id/like", nil, deviceIDHeader, "device")
		if rec := serveRequest(h, likeReq); rec.Code != http.StatusNotFound {
			t.Fatalf("pending public %s like should be 404, got %d", method, rec.Code)
		}
	}

	if rec := doRequest(h, http.MethodGet, "/api/admin/media/pending-id/thumbnail", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("pending admin thumbnail should require auth, got %d", rec.Code)
	}

	listReq := newTestRequest(http.MethodGet, "/api/admin/media?status=pending", nil)
	listReq.AddCookie(sess)
	listRec := serveRequest(h, listReq)
	var pending galleryResponse
	_ = json.Unmarshal(listRec.Body.Bytes(), &pending)
	if len(pending.Items) != 1 || pending.Items[0].ID != "pending-id" {
		t.Fatalf("unexpected pending queue: %+v", pending.Items)
	}

	approveReq := authedRequest(h, http.MethodPost, "/api/admin/media/bulk-approve", []byte(`{"ids":["pending-id"]}`), sess, csrfCookie, token)
	approveRec := serveRequest(h, approveReq)
	if approveRec.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", approveRec.Code, approveRec.Body.String())
	}
	galleryRec = doRequest(h, http.MethodGet, "/api/gallery", nil)
	_ = json.Unmarshal(galleryRec.Body.Bytes(), &gallery)
	if len(gallery.Items) != 1 || gallery.Items[0].ID != "pending-id" {
		t.Fatalf("approved item missing from gallery: %+v", gallery.Items)
	}
}

func TestModeration_DisablingAutoApprovesAllAndPreservesExpiry(t *testing.T) {
	h := newTestHarness(t)
	sess, csrfCookie, token := adminLogin(t, h, h.cfg.AdminPassword)
	if _, err := h.store.SetApprovalRequired(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	insertTestMedia(t, h, "pending-active", "pending-active-sha", time.Now())
	insertTestMedia(t, h, "pending-trash", "pending-trash-sha", time.Now())
	if _, err := h.store.SetStatusBulk(t.Context(), []string{"pending-trash"}, models.StatusTrashed); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetConfig(t.Context(), store.ConfigKeyUploadExpiresAt, time.Now().Add(time.Hour).UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}

	disableReq := authedRequest(h, http.MethodPut, "/api/admin/moderation", []byte(`{"approvalRequired":false}`), sess, csrfCookie, token)
	disableRec := serveRequest(h, disableReq)
	if disableRec.Code != http.StatusOK {
		t.Fatalf("disable moderation: %d %s", disableRec.Code, disableRec.Body.String())
	}
	var response moderationConfigResponse
	_ = json.Unmarshal(disableRec.Body.Bytes(), &response)
	if response.ApprovalRequired || response.AutoApproved != 2 {
		t.Fatalf("unexpected disable response: %+v", response)
	}
	if _, ok, _ := h.store.GetConfig(t.Context(), store.ConfigKeyUploadExpiresAt); !ok {
		t.Fatal("moderation update cleared upload expiry")
	}
	trashed, err := h.store.GetByID(t.Context(), "pending-trash", "")
	if err != nil || trashed.ApprovedAt == nil || trashed.Status != models.StatusTrashed {
		t.Fatalf("trashed pending item was not approved in place: %+v %v", trashed, err)
	}
}

func TestModerationMutationsRequireCSRF(t *testing.T) {
	h := newTestHarness(t)
	sess, _, _ := adminLogin(t, h, h.cfg.AdminPassword)
	req := newTestRequest(http.MethodPut, "/api/admin/moderation", []byte(`{"approvalRequired":true}`))
	req.AddCookie(sess)
	if rec := serveRequest(h, req); rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}
