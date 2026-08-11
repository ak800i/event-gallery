package tussidecar

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeInfo(t *testing.T, dir, id, body string) string {
	t.Helper()
	path := filepath.Join(dir, id+".info")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestParseValidSidecar(t *testing.T) {
	dir := t.TempDir()
	dataPath := filepath.Join(dir, "abc")
	if err := os.WriteFile(dataPath, []byte("hello"), 0o600); err != nil {
		t.Fatalf("write data: %v", err)
	}
	path := writeInfo(t, dir, "abc", `{"ID":"abc","Size":5,"Offset":5,"MetaData":{"filename":"a.jpg"},"Storage":{"Type":"filestore","Path":"`+strings.ReplaceAll(dataPath, `\`, `\\`)+`"}}`)

	info, err := Parse(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if info.ID != "abc" || info.Size != 5 || info.MetaData["filename"] != "a.jpg" {
		t.Errorf("unexpected info: %+v", info)
	}
}

func TestParseRejectsOversizedSidecar(t *testing.T) {
	dir := t.TempDir()
	path := writeInfo(t, dir, "big", `{"ID":"big",`+strings.Repeat(" ", maxSidecarBytes)+`"Size":1}`)
	_, err := Parse(path)
	if err == nil {
		t.Fatal("expected oversized sidecar to be rejected")
	}
	if !errors.Is(err, ErrMalformed) {
		t.Errorf("expected ErrMalformed, got %v", err)
	}
}

// The payload here is a complete, otherwise-valid sidecar followed by padding
// that pushes it past the bound. Truncating the read still leaves parseable
// JSON, so only the explicit size check can reject it: delete that check and
// this test sees a successful parse rather than a different error.
func TestParseRejectsOversizedSidecarThatIsOtherwiseValid(t *testing.T) {
	dir := t.TempDir()
	body := `{"ID":"padded","Size":1,"Storage":{"Type":"filestore","Path":"/x"}}` + strings.Repeat(" ", maxSidecarBytes)
	if len(body) <= maxSidecarBytes {
		t.Fatalf("test payload must exceed the bound, got %d bytes", len(body))
	}
	path := writeInfo(t, dir, "padded", body)

	info, err := Parse(path)
	if err == nil {
		t.Fatalf("expected oversized sidecar to be rejected, got %+v", info)
	}
	if !errors.Is(err, ErrMalformed) {
		t.Errorf("expected ErrMalformed, got %v", err)
	}
}

func TestParseRejectsWrongStorageType(t *testing.T) {
	dir := t.TempDir()
	path := writeInfo(t, dir, "s3", `{"ID":"s3","Size":1,"Storage":{"Type":"s3store","Path":"/x"}}`)
	_, err := Parse(path)
	if err == nil {
		t.Fatal("expected non-filestore sidecar to be rejected")
	}
	if !errors.Is(err, ErrMalformed) {
		t.Errorf("expected ErrMalformed, got %v", err)
	}
}

func TestParseRejectsDeferredSize(t *testing.T) {
	dir := t.TempDir()
	path := writeInfo(t, dir, "deferred", `{"ID":"deferred","Size":1,"SizeIsDeferred":true,"Storage":{"Type":"filestore","Path":"/x"}}`)
	_, err := Parse(path)
	if err == nil {
		t.Fatal("expected deferred-size sidecar to be rejected")
	}
	if !errors.Is(err, ErrMalformed) {
		t.Errorf("expected ErrMalformed, got %v", err)
	}
}

func TestParseRejectsNonPositiveSize(t *testing.T) {
	dir := t.TempDir()
	path := writeInfo(t, dir, "empty", `{"ID":"empty","Size":0,"Storage":{"Type":"filestore","Path":"/x"}}`)
	_, err := Parse(path)
	if err == nil {
		t.Fatal("expected non-positive size to be rejected")
	}
	if !errors.Is(err, ErrMalformed) {
		t.Errorf("expected ErrMalformed, got %v", err)
	}
}

func TestParseRejectsInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := writeInfo(t, dir, "broken", `{"ID":"broken","Size":`)
	_, err := Parse(path)
	if err == nil {
		t.Fatal("expected invalid JSON to be rejected")
	}
	if !errors.Is(err, ErrMalformed) {
		t.Errorf("expected ErrMalformed, got %v", err)
	}
}

func TestParseRejectsEmptyUploadID(t *testing.T) {
	dir := t.TempDir()
	path := writeInfo(t, dir, "noid", `{"ID":"","Size":1,"Storage":{"Type":"filestore","Path":"/x"}}`)
	_, err := Parse(path)
	if err == nil {
		t.Fatal("expected empty upload id to be rejected")
	}
	if !errors.Is(err, ErrMalformed) {
		t.Errorf("expected ErrMalformed, got %v", err)
	}
}

func TestParseRejectsEmptyStoragePath(t *testing.T) {
	dir := t.TempDir()
	path := writeInfo(t, dir, "nopath", `{"ID":"nopath","Size":1,"Storage":{"Type":"filestore","Path":""}}`)
	_, err := Parse(path)
	if err == nil {
		t.Fatal("expected empty storage path to be rejected")
	}
	if !errors.Is(err, ErrMalformed) {
		t.Errorf("expected ErrMalformed, got %v", err)
	}
}

// A missing sidecar is not a malformed one: callers must be able to tell the
// two apart, so the open error is reported unwrapped.
func TestParseMissingSidecarIsNotMalformed(t *testing.T) {
	dir := t.TempDir()
	_, err := Parse(filepath.Join(dir, "absent.info"))
	if err == nil {
		t.Fatal("expected missing sidecar to error")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected not-exist error, got %v", err)
	}
	if errors.Is(err, ErrMalformed) {
		t.Errorf("missing sidecar must not be reported as malformed, got %v", err)
	}
}
