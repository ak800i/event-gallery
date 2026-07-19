package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func adminLogin(t *testing.T, h *testHarness, password string) (*http.Cookie, *http.Cookie, string) {
	t.Helper()
	body := []byte(`{"password":"` + password + `"}`)
	rec := doRequest(h, http.MethodPost, "/api/admin/login", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("login failed: %d %s", rec.Code, rec.Body.String())
	}
	var sessionCookie, csrfCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		switch c.Name {
		case sessionCookieName:
			sessionCookie = c
		case csrfCookieName:
			csrfCookie = c
		}
	}
	if sessionCookie == nil || csrfCookie == nil {
		t.Fatalf("expected both session and csrf cookies to be set")
	}
	var resp map[string]string
	json.Unmarshal(rec.Body.Bytes(), &resp)
	return sessionCookie, csrfCookie, resp["csrfToken"]
}

func authedRequest(h *testHarness, method, target string, body []byte, sess, csrfCookie *http.Cookie, csrfHeader string) *http.Request {
	req := newTestRequest(method, target, body)
	req.AddCookie(sess)
	if csrfCookie != nil {
		req.AddCookie(csrfCookie)
	}
	if csrfHeader != "" {
		req.Header.Set(csrfHeaderName, csrfHeader)
	}
	return req
}

func TestAdminLogin_Success(t *testing.T) {
	h := newTestHarness(t)
	sess, csrf, token := adminLogin(t, h, h.cfg.AdminPassword)
	if sess.Value == "" || csrf.Value == "" || token == "" {
		t.Fatal("expected non-empty session/csrf values")
	}
}

func TestAdminLogin_WrongPassword(t *testing.T) {
	h := newTestHarness(t)
	rec := doRequest(h, http.MethodPost, "/api/admin/login", []byte(`{"password":"wrong-password"}`))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestAdminLogin_RateLimited(t *testing.T) {
	h := newTestHarness(t)
	var lastCode int
	for i := 0; i < 10; i++ {
		rec := doRequest(h, http.MethodPost, "/api/admin/login", []byte(`{"password":"wrong"}`))
		lastCode = rec.Code
	}
	if lastCode != http.StatusTooManyRequests {
		t.Fatalf("expected eventual 429 from repeated login attempts, got %d", lastCode)
	}
}

func TestAdminSession_RequiresAuth(t *testing.T) {
	h := newTestHarness(t)
	rec := doRequest(h, http.MethodGet, "/api/admin/session", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without session, got %d", rec.Code)
	}
}

func TestAdminSession_WithValidCookie(t *testing.T) {
	h := newTestHarness(t)
	sess, _, _ := adminLogin(t, h, h.cfg.AdminPassword)

	req := newTestRequest(http.MethodGet, "/api/admin/session", nil)
	req.AddCookie(sess)
	rec := serveRequest(h, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestAdminLogout_RequiresCSRF(t *testing.T) {
	h := newTestHarness(t)
	sess, _, _ := adminLogin(t, h, h.cfg.AdminPassword)

	// Missing CSRF header should be rejected.
	req := newTestRequest(http.MethodPost, "/api/admin/logout", nil)
	req.AddCookie(sess)
	rec := serveRequest(h, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 missing csrf, got %d", rec.Code)
	}
}

func TestAdminLogout_WithValidCSRF(t *testing.T) {
	h := newTestHarness(t)
	sess, csrfCookie, token := adminLogin(t, h, h.cfg.AdminPassword)

	req := authedRequest(h, http.MethodPost, "/api/admin/logout", nil, sess, csrfCookie, token)
	rec := serveRequest(h, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Session should now be invalid.
	req2 := newTestRequest(http.MethodGet, "/api/admin/session", nil)
	req2.AddCookie(sess)
	rec2 := serveRequest(h, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("expected session invalidated after logout, got %d", rec2.Code)
	}
}

func TestAdminBulkDeleteAndRestore(t *testing.T) {
	h := newTestHarness(t)
	insertTestMedia(t, h, "id1", "sha1", time.Now())
	insertTestMedia(t, h, "id2", "sha2", time.Now())
	sess, csrfCookie, token := adminLogin(t, h, h.cfg.AdminPassword)

	delReq := authedRequest(h, http.MethodPost, "/api/admin/media/bulk-delete", []byte(`{"ids":["id1","id2"]}`), sess, csrfCookie, token)
	delRec := serveRequest(h, delReq)
	if delRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", delRec.Code, delRec.Body.String())
	}

	// Gallery should now be empty (both trashed).
	galRec := doRequest(h, http.MethodGet, "/api/gallery", nil)
	var galResp galleryResponse
	json.Unmarshal(galRec.Body.Bytes(), &galResp)
	if len(galResp.Items) != 0 {
		t.Fatalf("expected empty gallery after bulk delete, got %d items", len(galResp.Items))
	}

	// Admin listing with status=trashed should show both.
	listReq := newTestRequest(http.MethodGet, "/api/admin/media?status=trashed", nil)
	listReq.AddCookie(sess)
	listRec := serveRequest(h, listReq)
	var listResp galleryResponse
	json.Unmarshal(listRec.Body.Bytes(), &listResp)
	if len(listResp.Items) != 2 {
		t.Fatalf("expected 2 trashed items, got %d", len(listResp.Items))
	}

	// Restore id1.
	restoreReq := authedRequest(h, http.MethodPost, "/api/admin/media/bulk-restore", []byte(`{"ids":["id1"]}`), sess, csrfCookie, token)
	restoreRec := serveRequest(h, restoreReq)
	if restoreRec.Code != http.StatusOK {
		t.Fatalf("expected 200 restoring, got %d", restoreRec.Code)
	}

	galRec2 := doRequest(h, http.MethodGet, "/api/gallery", nil)
	var galResp2 galleryResponse
	json.Unmarshal(galRec2.Body.Bytes(), &galResp2)
	if len(galResp2.Items) != 1 || galResp2.Items[0].ID != "id1" {
		t.Fatalf("expected id1 restored, got %+v", galResp2.Items)
	}

	// Audit log should have entries for delete/restore/login.
	auditReq := newTestRequest(http.MethodGet, "/api/admin/audit-log", nil)
	auditReq.AddCookie(sess)
	auditRec := serveRequest(h, auditReq)
	var auditResp auditLogResponse
	json.Unmarshal(auditRec.Body.Bytes(), &auditResp)
	foundDelete, foundRestore, foundLogin := false, false, false
	for _, e := range auditResp.Entries {
		switch e.Action {
		case "delete":
			foundDelete = true
		case "restore":
			foundRestore = true
		case "login":
			foundLogin = true
		}
	}
	if !foundDelete || !foundRestore || !foundLogin {
		t.Fatalf("expected delete/restore/login audit entries, got %+v", auditResp.Entries)
	}
}

func TestAdminBulkDelete_RequiresCSRF(t *testing.T) {
	h := newTestHarness(t)
	insertTestMedia(t, h, "id1", "sha1", time.Now())
	sess, _, _ := adminLogin(t, h, h.cfg.AdminPassword)

	req := newTestRequest(http.MethodPost, "/api/admin/media/bulk-delete", []byte(`{"ids":["id1"]}`))
	req.AddCookie(sess)
	rec := serveRequest(h, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 without csrf, got %d", rec.Code)
	}
}

func TestAdminBulkDelete_EmptyIDs(t *testing.T) {
	h := newTestHarness(t)
	sess, csrfCookie, token := adminLogin(t, h, h.cfg.AdminPassword)
	req := authedRequest(h, http.MethodPost, "/api/admin/media/bulk-delete", []byte(`{"ids":[]}`), sess, csrfCookie, token)
	rec := serveRequest(h, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty ids, got %d", rec.Code)
	}
}

func TestAdminConfig_SetAndClearExpiry(t *testing.T) {
	h := newTestHarness(t)
	sess, csrfCookie, token := adminLogin(t, h, h.cfg.AdminPassword)

	future := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	setReq := authedRequest(h, http.MethodPut, "/api/admin/config", []byte(`{"uploadExpiresAt":"`+future+`"}`), sess, csrfCookie, token)
	setRec := serveRequest(h, setReq)
	if setRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", setRec.Code, setRec.Body.String())
	}

	// Public config should still show uploads enabled (future expiry).
	pubRec := doRequest(h, http.MethodGet, "/api/config/public", nil)
	var pubResp publicConfigResponse
	json.Unmarshal(pubRec.Body.Bytes(), &pubResp)
	if !pubResp.UploadsEnabled {
		t.Error("expected uploads still enabled with future expiry")
	}
	if pubResp.UploadExpiresAt == nil {
		t.Error("expected uploadExpiresAt to be set")
	}

	// Clear the expiry.
	clearReq := authedRequest(h, http.MethodPut, "/api/admin/config", []byte(`{"uploadExpiresAt":""}`), sess, csrfCookie, token)
	clearRec := serveRequest(h, clearReq)
	if clearRec.Code != http.StatusOK {
		t.Fatalf("expected 200 clearing expiry, got %d", clearRec.Code)
	}
	pubRec2 := doRequest(h, http.MethodGet, "/api/config/public", nil)
	var pubResp2 publicConfigResponse
	json.Unmarshal(pubRec2.Body.Bytes(), &pubResp2)
	if pubResp2.UploadExpiresAt != nil {
		t.Error("expected uploadExpiresAt cleared")
	}
}

func newTestRequest(method, target string, body []byte) *http.Request {
	return newRequestWithHeader(method, target, body, "Accept", "application/json")
}
