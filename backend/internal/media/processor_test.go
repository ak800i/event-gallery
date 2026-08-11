package media

import (
	"testing"
)

func TestMimeToExt_AVIF(t *testing.T) {
	if got := mimeToExt["image/avif"]; got != ".avif" {
		t.Errorf("expected .avif for image/avif, got %q", got)
	}
}

// The stored name comes from sniffed content, not from whatever the client
// called the file. Only an unmapped type may fall back to the client's
// extension.
func TestExtensionForMIMEPrefersSniffedContent(t *testing.T) {
	if got := ExtensionForMIME("image/jpeg", "photo.png"); got != ".jpg" {
		t.Errorf("ExtensionForMIME(image/jpeg) = %q, want .jpg", got)
	}
	if got := ExtensionForMIME("application/x-unmapped", "clip.mkv"); got != ".mkv" {
		t.Errorf("unmapped type must fall back to the filename, got %q", got)
	}
	if got := ExtensionForMIME("application/x-unmapped", "noextension"); got != "" {
		t.Errorf("no extension anywhere must stay empty, got %q", got)
	}
}
