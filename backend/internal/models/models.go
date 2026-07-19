// Package models defines the core domain types shared across the backend.
package models

import "time"

// MediaKind distinguishes photos from videos.
type MediaKind string

const (
	KindImage MediaKind = "image"
	KindVideo MediaKind = "video"
)

// MediaStatus tracks whether an item is visible in the gallery or in the
// admin trash awaiting restoration.
type MediaStatus string

const (
	StatusActive  MediaStatus = "active"
	StatusTrashed MediaStatus = "trashed"
)

// MediaItem represents one uploaded photo or video.
type MediaItem struct {
	ID               string
	OriginalFilename string
	StoredFilename   string
	Kind             MediaKind
	MimeType         string
	SizeBytes        int64
	SHA256           string
	Width            int
	Height           int
	DurationSeconds  float64
	HasThumbnail     bool
	CapturedAt       *time.Time
	UploadedAt       time.Time
	UploaderName     string
	UploaderIP       string
	Status           MediaStatus
	DeletedAt        *time.Time
	LikeCount        int
	LikedByDevice    bool
}

// AuditAction enumerates the kinds of events recorded in the audit log.
type AuditAction string

const (
	ActionUpload      AuditAction = "upload"
	ActionDelete      AuditAction = "delete"
	ActionRestore     AuditAction = "restore"
	ActionLogin       AuditAction = "login"
	ActionLoginFailed AuditAction = "login_failed"
	ActionConfig      AuditAction = "config"
	ActionLogout      AuditAction = "logout"
)

// AuditEntry is one row in the audit log.
type AuditEntry struct {
	ID        int64
	Action    AuditAction
	Actor     string
	MediaID   string
	Filename  string
	Details   string
	CreatedAt time.Time
}

// GallerySort controls ordering of the public gallery feed.
type GallerySort string

const (
	SortUploaded GallerySort = "uploaded"
	SortCaptured GallerySort = "captured"
)

// SortOrder controls ascending vs descending ordering.
type SortOrder string

const (
	OrderAsc  SortOrder = "asc"
	OrderDesc SortOrder = "desc"
)
