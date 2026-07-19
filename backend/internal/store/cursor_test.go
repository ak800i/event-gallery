package store

import (
	"encoding/base64"
	"testing"
)

func TestCursorRoundTrip(t *testing.T) {
	c := &Cursor{SortKey: "2024-01-02T15:04:05.999999999Z", ID: "abc-123"}
	encoded := EncodeCursor(c)
	if encoded == "" {
		t.Fatal("expected non-empty encoded cursor")
	}
	decoded, err := DecodeCursor(encoded)
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if decoded.SortKey != c.SortKey || decoded.ID != c.ID {
		t.Errorf("round trip mismatch: got %+v want %+v", decoded, c)
	}
}

func TestDecodeCursor_Empty(t *testing.T) {
	c, err := DecodeCursor("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c != nil {
		t.Errorf("expected nil cursor for empty string")
	}
}

func TestDecodeCursor_Invalid(t *testing.T) {
	if _, err := DecodeCursor("not-valid-base64!!"); err == nil {
		t.Error("expected error for invalid base64")
	}
	// Valid base64 but not valid JSON payload.
	bogus := base64.RawURLEncoding.EncodeToString([]byte("not json"))
	if _, err := DecodeCursor(bogus); err == nil {
		t.Error("expected error for invalid JSON payload")
	}
}

func TestEncodeCursor_Nil(t *testing.T) {
	if got := EncodeCursor(nil); got != "" {
		t.Errorf("expected empty string for nil cursor, got %q", got)
	}
}
