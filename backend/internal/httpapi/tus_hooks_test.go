package httpapi

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"wedding-gallery/backend/internal/media"
)

func writeTestJPEGFile(t *testing.T, path string, w, h int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 50, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write jpeg: %v", err)
	}
}

func hookRequestBody(t *testing.T, req tusHookRequest) []byte {
	t.Helper()
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal hook request: %v", err)
	}
	return b
}

func TestTusHook_UnauthorizedWithoutSecret(t *testing.T) {
	h := newTestHarness(t)
	req := newTestRequest(http.MethodPost, "/api/internal/tus-hooks", []byte(`{"Type":"post-create"}`))
	rec := serveRequest(h, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestTusHook_PreCreate_RejectsOversized(t *testing.T) {
	h := newTestHarness(t)
	body := hookRequestBody(t, tusHookRequest{
		Type: "pre-create",
		Event: tusHookEvent{
			Upload: tusHookUpload{
				Size:     h.cfg.MaxUploadBytes + 1,
				MetaData: map[string]string{"filename": "big.jpg"},
			},
		},
	})
	req := newRequestWithHeader(http.MethodPost, "/api/internal/tus-hooks", body, internalProxySecretHeader, h.cfg.TusHookSecret)
	rec := serveRequest(h, req)
	if rec.Code != http.StatusOK { // hook endpoint always 200s; rejection is in the JSON body
		t.Fatalf("expected 200 (hook envelope), got %d", rec.Code)
	}
	var resp tusHookResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if !resp.RejectUpload {
		t.Fatal("expected RejectUpload true for oversized upload")
	}
}

func TestTusHook_PreCreate_RejectsMissingFilename(t *testing.T) {
	h := newTestHarness(t)
	body := hookRequestBody(t, tusHookRequest{
		Type: "pre-create",
		Event: tusHookEvent{
			Upload: tusHookUpload{Size: 100, MetaData: map[string]string{}},
		},
	})
	req := newRequestWithHeader(http.MethodPost, "/api/internal/tus-hooks", body, internalProxySecretHeader, h.cfg.TusHookSecret)
	rec := serveRequest(h, req)
	var resp tusHookResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if !resp.RejectUpload {
		t.Fatal("expected RejectUpload true for missing filename")
	}
}

func TestTusHook_PreCreate_AllowsValid(t *testing.T) {
	h := newTestHarness(t)
	body := hookRequestBody(t, tusHookRequest{
		Type: "pre-create",
		Event: tusHookEvent{
			Upload: tusHookUpload{Size: 100, MetaData: map[string]string{"filename": "a.jpg"}},
		},
	})
	req := newRequestWithHeader(http.MethodPost, "/api/internal/tus-hooks", body, internalProxySecretHeader, h.cfg.TusHookSecret)
	rec := serveRequest(h, req)
	var resp tusHookResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.RejectUpload {
		t.Fatal("expected valid upload to be allowed")
	}
}

func TestTusHook_PostFinish_ProcessesAndInsertsMedia(t *testing.T) {
	h := newTestHarness(t)
	dir := t.TempDir()
	dataPath := filepath.Join(dir, "upload-abc123")
	writeTestJPEGFile(t, dataPath, 300, 200)
	infoPath := dataPath + ".info"
	os.WriteFile(infoPath, []byte(`{}`), 0o644)

	sha, err := media.SHA256File(dataPath)
	if err != nil {
		t.Fatalf("compute sha: %v", err)
	}

	body := hookRequestBody(t, tusHookRequest{
		Type: "post-finish",
		Event: tusHookEvent{
			Upload: tusHookUpload{
				ID:   "abc123",
				Size: 12345,
				MetaData: map[string]string{
					"filename":  "wedding.jpg",
					"guestName": "Alice",
					"sha256":    sha,
				},
				Storage: tusHookStorage{Type: "filestore", Path: dataPath},
			},
			HTTPRequest: tusHookHTTPRequest{
				Header: map[string][]string{clientIPHeader: {"203.0.113.5"}},
			},
		},
	})
	req := newRequestWithHeader(http.MethodPost, "/api/internal/tus-hooks", body, internalProxySecretHeader, h.cfg.TusHookSecret)
	rec := serveRequest(h, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// The gallery should now show the new item.
	galRec := doRequest(h, http.MethodGet, "/api/gallery", nil)
	var galResp galleryResponse
	json.Unmarshal(galRec.Body.Bytes(), &galResp)
	if len(galResp.Items) != 1 {
		t.Fatalf("expected 1 item in gallery, got %d", len(galResp.Items))
	}
	item := galResp.Items[0]
	if item.OriginalName != "wedding.jpg" || item.UploaderName != "Alice" {
		t.Errorf("unexpected item: %+v", item)
	}
	if item.Width != 300 || item.Height != 200 {
		t.Errorf("expected dimensions 300x200, got %dx%d", item.Width, item.Height)
	}

	// The tusd info file should have been cleaned up.
	if _, err := os.Stat(infoPath); !os.IsNotExist(err) {
		t.Error("expected tusd .info file to be removed")
	}
	// The original data path should no longer exist (moved to permanent storage).
	if _, err := os.Stat(dataPath); !os.IsNotExist(err) {
		t.Error("expected tusd data file to be moved away")
	}
}

func TestTusHook_PostFinish_RejectsChecksumMismatch(t *testing.T) {
	h := newTestHarness(t)
	dir := t.TempDir()
	dataPath := filepath.Join(dir, "upload-mismatch")
	writeTestJPEGFile(t, dataPath, 100, 100)

	body := hookRequestBody(t, tusHookRequest{
		Type: "post-finish",
		Event: tusHookEvent{
			Upload: tusHookUpload{
				ID:   "mismatch",
				Size: 100,
				MetaData: map[string]string{
					"filename": "wedding.jpg",
					"sha256":   "0000000000000000000000000000000000000000000000000000000000000",
				},
				Storage: tusHookStorage{Type: "filestore", Path: dataPath},
			},
		},
	})
	req := newRequestWithHeader(http.MethodPost, "/api/internal/tus-hooks", body, internalProxySecretHeader, h.cfg.TusHookSecret)
	rec := serveRequest(h, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	galRec := doRequest(h, http.MethodGet, "/api/gallery", nil)
	var galResp galleryResponse
	json.Unmarshal(galRec.Body.Bytes(), &galResp)
	if len(galResp.Items) != 0 {
		t.Fatalf("expected checksum-mismatched upload to be rejected, got %d items", len(galResp.Items))
	}
}

func TestTusHook_PostFinish_RejectsUnsupportedType(t *testing.T) {
	h := newTestHarness(t)
	dir := t.TempDir()
	dataPath := filepath.Join(dir, "upload-bad")
	os.WriteFile(dataPath, []byte("not a real media file at all"), 0o644)

	body := hookRequestBody(t, tusHookRequest{
		Type: "post-finish",
		Event: tusHookEvent{
			Upload: tusHookUpload{
				ID:       "bad",
				Size:     100,
				MetaData: map[string]string{"filename": "notreal.jpg"},
				Storage:  tusHookStorage{Type: "filestore", Path: dataPath},
			},
		},
	})
	req := newRequestWithHeader(http.MethodPost, "/api/internal/tus-hooks", body, internalProxySecretHeader, h.cfg.TusHookSecret)
	rec := serveRequest(h, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	galRec := doRequest(h, http.MethodGet, "/api/gallery", nil)
	var galResp galleryResponse
	json.Unmarshal(galRec.Body.Bytes(), &galResp)
	if len(galResp.Items) != 0 {
		t.Fatalf("expected unsupported type to be rejected, got %d items", len(galResp.Items))
	}
}

func TestTusHook_PostFinish_DuplicateIgnored(t *testing.T) {
	h := newTestHarness(t)
	dir := t.TempDir()

	// First upload.
	firstPath := filepath.Join(dir, "upload-first")
	writeTestJPEGFile(t, firstPath, 150, 150)
	firstBody := hookRequestBody(t, tusHookRequest{
		Type: "post-finish",
		Event: tusHookEvent{
			Upload: tusHookUpload{
				ID:       "first",
				Size:     100,
				MetaData: map[string]string{"filename": "one.jpg"},
				Storage:  tusHookStorage{Type: "filestore", Path: firstPath},
			},
		},
	})
	req1 := newRequestWithHeader(http.MethodPost, "/api/internal/tus-hooks", firstBody, internalProxySecretHeader, h.cfg.TusHookSecret)
	serveRequest(h, req1)

	// Second upload with identical bytes (simulate two guests uploading the
	// same photo).
	secondPath := filepath.Join(dir, "upload-second")
	writeTestJPEGFile(t, secondPath, 150, 150)
	secondBody := hookRequestBody(t, tusHookRequest{
		Type: "post-finish",
		Event: tusHookEvent{
			Upload: tusHookUpload{
				ID:       "second",
				Size:     100,
				MetaData: map[string]string{"filename": "two.jpg"},
				Storage:  tusHookStorage{Type: "filestore", Path: secondPath},
			},
		},
	})
	req2 := newRequestWithHeader(http.MethodPost, "/api/internal/tus-hooks", secondBody, internalProxySecretHeader, h.cfg.TusHookSecret)
	rec2 := serveRequest(h, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec2.Code)
	}

	galRec := doRequest(h, http.MethodGet, "/api/gallery", nil)
	var galResp galleryResponse
	json.Unmarshal(galRec.Body.Bytes(), &galResp)
	if len(galResp.Items) != 1 {
		t.Fatalf("expected exactly 1 item (duplicate ignored), got %d", len(galResp.Items))
	}
}
