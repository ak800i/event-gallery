package store

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// Cursor is an opaque pagination cursor combining the sort key value (an
// RFC3339Nano timestamp string) with the item ID as a tiebreaker so pages
// remain stable even when multiple items share the same timestamp.
type Cursor struct {
	SortKey string `json:"k"`
	ID      string `json:"i"`
}

// EncodeCursor serializes a cursor to an opaque URL-safe string.
func EncodeCursor(c *Cursor) string {
	if c == nil {
		return ""
	}
	b, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeCursor parses an opaque cursor string produced by EncodeCursor. An
// empty string decodes to a nil cursor (first page).
func DecodeCursor(s string) (*Cursor, error) {
	if s == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("invalid cursor encoding: %w", err)
	}
	var c Cursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("invalid cursor payload: %w", err)
	}
	return &c, nil
}
