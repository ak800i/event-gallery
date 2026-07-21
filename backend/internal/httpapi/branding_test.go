package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"event-gallery/backend/internal/store"
)

func TestPublicConfig_IncludesDefaultBranding(t *testing.T) {
	h := newTestHarness(t)
	rec := doRequest(h, http.MethodGet, "/api/config/public", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp publicConfigResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	want := defaultBrandingConfig()
	if resp.Branding != want {
		t.Fatalf("unexpected default branding:\n got: %+v\nwant: %+v", resp.Branding, want)
	}
}

func TestAdminBranding_UpdateIsPublicAndPreservesUploadExpiry(t *testing.T) {
	h := newTestHarness(t)
	sess, csrfCookie, token := adminLogin(t, h, h.cfg.AdminPassword)

	future := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	expiryReq := authedRequest(h, http.MethodPut, "/api/admin/config", []byte(`{"uploadExpiresAt":"`+future+`"}`), sess, csrfCookie, token)
	if rec := serveRequest(h, expiryReq); rec.Code != http.StatusOK {
		t.Fatalf("set expiry: %d %s", rec.Code, rec.Body.String())
	}

	value := defaultBrandingConfig()
	value.PageTitle = "Sam & Alex"
	value.PageSubtitle = "<strong>This stays plain text</strong>"
	value.UploadHelperText = "Maximum {maxSize}; interrupted uploads continue"
	value.GalleryLoadingText = ""
	value.BackgroundColor = "#123456"
	value.BorderColor = "#abcdef"
	body, _ := json.Marshal(value)
	updateReq := authedRequest(h, http.MethodPut, "/api/admin/branding", body, sess, csrfCookie, token)
	updateRec := serveRequest(h, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("update branding: %d %s", updateRec.Code, updateRec.Body.String())
	}

	pubRec := doRequest(h, http.MethodGet, "/api/config/public", nil)
	var pub publicConfigResponse
	if err := json.Unmarshal(pubRec.Body.Bytes(), &pub); err != nil {
		t.Fatalf("decode public config: %v", err)
	}
	if pub.Branding.PageTitle != "Sam & Alex" || pub.Branding.BackgroundColor != "#123456" {
		t.Fatalf("public branding did not reflect update: %+v", pub.Branding)
	}
	if pub.Branding.PageSubtitle != "<strong>This stays plain text</strong>" {
		t.Fatalf("text should be preserved literally, got %q", pub.Branding.PageSubtitle)
	}
	if pub.UploadExpiresAt == nil {
		t.Fatal("branding-only update unexpectedly cleared upload expiry")
	}
}

func TestAdminBranding_RejectsInvalidColorWithoutOverwriting(t *testing.T) {
	h := newTestHarness(t)
	sess, csrfCookie, token := adminLogin(t, h, h.cfg.AdminPassword)

	value := defaultBrandingConfig()
	value.PrimaryColor = "red; background:url(evil)"
	body, _ := json.Marshal(value)
	req := authedRequest(h, http.MethodPut, "/api/admin/branding", body, sess, csrfCookie, token)
	rec := serveRequest(h, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}

	getReq := newTestRequest(http.MethodGet, "/api/admin/branding", nil)
	getReq.AddCookie(sess)
	getRec := serveRequest(h, getReq)
	var got brandingConfig
	_ = json.Unmarshal(getRec.Body.Bytes(), &got)
	if got != defaultBrandingConfig() {
		t.Fatalf("invalid update should not be saved: %+v", got)
	}
}

func TestPublicConfig_InvalidStoredBrandingFallsBackToDefaults(t *testing.T) {
	h := newTestHarness(t)
	if err := h.store.SetConfig(context.Background(), store.ConfigKeyBranding, `{not-json`); err != nil {
		t.Fatalf("set corrupt branding: %v", err)
	}
	rec := doRequest(h, http.MethodGet, "/api/config/public", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected public config to fail open, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp publicConfigResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Branding != defaultBrandingConfig() {
		t.Fatalf("expected default branding fallback, got %+v", resp.Branding)
	}
}

func TestAdminBranding_RejectsEmptyCriticalControlText(t *testing.T) {
	h := newTestHarness(t)
	sess, csrfCookie, token := adminLogin(t, h, h.cfg.AdminPassword)
	value := defaultBrandingConfig()
	value.ChangeNameText = ""
	body, _ := json.Marshal(value)
	req := authedRequest(h, http.MethodPut, "/api/admin/branding", body, sess, csrfCookie, token)
	rec := serveRequest(h, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty control text, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAdminBranding_ResetRestoresDefaults(t *testing.T) {
	h := newTestHarness(t)
	sess, csrfCookie, token := adminLogin(t, h, h.cfg.AdminPassword)

	value := defaultBrandingConfig()
	value.PageTitle = "Custom"
	body, _ := json.Marshal(value)
	updateReq := authedRequest(h, http.MethodPut, "/api/admin/branding", body, sess, csrfCookie, token)
	if rec := serveRequest(h, updateReq); rec.Code != http.StatusOK {
		t.Fatalf("update branding: %d %s", rec.Code, rec.Body.String())
	}

	resetReq := authedRequest(h, http.MethodDelete, "/api/admin/branding", nil, sess, csrfCookie, token)
	resetRec := serveRequest(h, resetReq)
	if resetRec.Code != http.StatusOK {
		t.Fatalf("reset branding: %d %s", resetRec.Code, resetRec.Body.String())
	}
	var got brandingConfig
	_ = json.Unmarshal(resetRec.Body.Bytes(), &got)
	if got != defaultBrandingConfig() {
		t.Fatalf("unexpected reset branding: %+v", got)
	}
}

func TestAdminBranding_UpdateRequiresCSRF(t *testing.T) {
	h := newTestHarness(t)
	sess, _, _ := adminLogin(t, h, h.cfg.AdminPassword)
	body, _ := json.Marshal(defaultBrandingConfig())
	req := newTestRequest(http.MethodPut, "/api/admin/branding", body)
	req.AddCookie(sess)
	rec := serveRequest(h, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 without CSRF, got %d", rec.Code)
	}
}
