package tussidecar

import (
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
	if _, err := Parse(path); err == nil {
		t.Fatal("expected oversized sidecar to be rejected")
	}
}

func TestParseRejectsWrongStorageType(t *testing.T) {
	dir := t.TempDir()
	path := writeInfo(t, dir, "s3", `{"ID":"s3","Size":1,"Storage":{"Type":"s3store","Path":"/x"}}`)
	if _, err := Parse(path); err == nil {
		t.Fatal("expected non-filestore sidecar to be rejected")
	}
}

func TestParseRejectsDeferredSize(t *testing.T) {
	dir := t.TempDir()
	path := writeInfo(t, dir, "deferred", `{"ID":"deferred","Size":1,"SizeIsDeferred":true,"Storage":{"Type":"filestore","Path":"/x"}}`)
	if _, err := Parse(path); err == nil {
		t.Fatal("expected deferred-size sidecar to be rejected")
	}
}

func TestParseRejectsNonPositiveSize(t *testing.T) {
	dir := t.TempDir()
	path := writeInfo(t, dir, "empty", `{"ID":"empty","Size":0,"Storage":{"Type":"filestore","Path":"/x"}}`)
	if _, err := Parse(path); err == nil {
		t.Fatal("expected non-positive size to be rejected")
	}
}
