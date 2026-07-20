package httpapi

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"wedding-gallery/backend/internal/models"
	"wedding-gallery/backend/internal/store"
)

const deviceIDHeader = "X-Device-Id"
const maxDeviceIDLength = 128

func deviceIDFromRequest(r *http.Request) string {
	id := strings.TrimSpace(r.Header.Get(deviceIDHeader))
	if len(id) > maxDeviceIDLength {
		id = id[:maxDeviceIDLength]
	}
	return id
}

type mediaItemDTO struct {
	ID              string  `json:"id"`
	OriginalName    string  `json:"originalFilename"`
	Kind            string  `json:"kind"`
	MimeType        string  `json:"mimeType"`
	SizeBytes       int64   `json:"sizeBytes"`
	Width           int     `json:"width,omitempty"`
	Height          int     `json:"height,omitempty"`
	DurationSeconds float64 `json:"durationSeconds,omitempty"`
	HasThumbnail    bool    `json:"hasThumbnail"`
	CapturedAt      *string `json:"capturedAt,omitempty"`
	UploadedAt      string  `json:"uploadedAt"`
	UploaderName    string  `json:"uploaderName"`
	LikeCount       int     `json:"likeCount"`
	LikedByDevice   bool    `json:"likedByDevice"`
	Status          string  `json:"status,omitempty"`
}

func toDTO(m models.MediaItem, includeStatus bool) mediaItemDTO {
	dto := mediaItemDTO{
		ID:              m.ID,
		OriginalName:    m.OriginalFilename,
		Kind:            string(m.Kind),
		MimeType:        m.MimeType,
		SizeBytes:       m.SizeBytes,
		Width:           m.Width,
		Height:          m.Height,
		DurationSeconds: m.DurationSeconds,
		HasThumbnail:    m.HasThumbnail,
		UploadedAt:      m.UploadedAt.UTC().Format(time.RFC3339),
		UploaderName:    m.UploaderName,
		LikeCount:       m.LikeCount,
		LikedByDevice:   m.LikedByDevice,
	}
	if m.CapturedAt != nil {
		s := m.CapturedAt.UTC().Format(time.RFC3339)
		dto.CapturedAt = &s
	}
	if includeStatus {
		dto.Status = string(m.Status)
	}
	return dto
}

type galleryResponse struct {
	Items      []mediaItemDTO `json:"items"`
	NextCursor string         `json:"nextCursor,omitempty"`
}

func parseLimit(r *http.Request, def, max int) int {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

// handleGallery serves one page of the public gallery feed, sorted by
// upload time or capture time, with cursor-based infinite scroll.
func (s *Server) handleGallery(w http.ResponseWriter, r *http.Request) {
	sortParam := models.GallerySort(r.URL.Query().Get("sort"))
	if sortParam != models.SortCaptured {
		sortParam = models.SortUploaded
	}
	orderParam := models.SortOrder(r.URL.Query().Get("order"))
	if orderParam != models.OrderAsc {
		orderParam = models.OrderDesc
	}
	cursor, err := store.DecodeCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid cursor")
		return
	}
	limit := parseLimit(r, 30, 100)
	deviceID := deviceIDFromRequest(r)

	items, nextCursor, err := s.store.ListGallery(r.Context(), store.ListGalleryParams{
		Sort: sortParam, Order: orderParam, Cursor: cursor, Limit: limit, DeviceID: deviceID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load gallery")
		return
	}

	dtos := make([]mediaItemDTO, 0, len(items))
	for _, item := range items {
		dtos = append(dtos, toDTO(item, false))
	}
	writeJSON(w, http.StatusOK, galleryResponse{Items: dtos, NextCursor: nextCursor})
}

type publicConfigResponse struct {
	UploadsEnabled    bool     `json:"uploadsEnabled"`
	UploadExpiresAt   *string  `json:"uploadExpiresAt,omitempty"`
	MaxUploadBytes    int64    `json:"maxUploadBytes"`
	UploadConcurrency int      `json:"uploadConcurrency"`
	AllowedImageMime  []string `json:"allowedImageMimeTypes"`
	AllowedVideoMime  []string `json:"allowedVideoMimeTypes"`
	GuestNameMaxLen   int      `json:"guestNameMaxLength"`
}

func (s *Server) handlePublicConfig(w http.ResponseWriter, r *http.Request) {
	closed, err := s.uploadsClosed(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load configuration")
		return
	}
	resp := publicConfigResponse{
		UploadsEnabled:    !closed,
		MaxUploadBytes:    s.cfg.MaxUploadBytes,
		UploadConcurrency: s.cfg.UploadConcurrencyPerIP,
		AllowedImageMime:  s.cfg.AllowedImageMIMEs,
		AllowedVideoMime:  s.cfg.AllowedVideoMIMEs,
		GuestNameMaxLen:   s.cfg.GuestNameMaxLength,
	}
	if value, ok, err := s.store.GetConfig(r.Context(), store.ConfigKeyUploadExpiresAt); err == nil && ok && value != "" {
		resp.UploadExpiresAt = &value
	}
	writeJSON(w, http.StatusOK, resp)
}

type uploadCheckRequest struct {
	SHA256   string `json:"sha256"`
	Size     int64  `json:"size"`
	Filename string `json:"filename"`
}

type uploadCheckResponse struct {
	Duplicate bool   `json:"duplicate"`
	MediaID   string `json:"mediaId,omitempty"`
}

// handleUploadCheck lets the client ask, before spending any bandwidth,
// whether a file with this exact whole-file SHA-256 has already been
// uploaded. This is a courtesy optimization; the authoritative duplicate
// check happens server-side again after the tus upload completes.
func (s *Server) handleUploadCheck(w http.ResponseWriter, r *http.Request) {
	var req uploadCheckRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.SHA256 = strings.ToLower(strings.TrimSpace(req.SHA256))
	if len(req.SHA256) != 64 {
		writeError(w, http.StatusBadRequest, "sha256 must be a 64-character hex digest")
		return
	}
	if req.Size <= 0 || req.Size > s.cfg.MaxUploadBytes {
		writeError(w, http.StatusBadRequest, "file size is invalid or exceeds the maximum allowed")
		return
	}

	existing, err := s.store.GetBySHA256(r.Context(), req.SHA256)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusOK, uploadCheckResponse{Duplicate: false})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to check for duplicates")
		return
	}
	writeJSON(w, http.StatusOK, uploadCheckResponse{Duplicate: true, MediaID: existing.ID})
}

func (s *Server) lookupActiveMedia(w http.ResponseWriter, r *http.Request) (*models.MediaItem, bool) {
	id := chi.URLParam(r, "id")
	item, err := s.store.GetByID(r.Context(), id, deviceIDFromRequest(r))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "media not found")
		return nil, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load media")
		return nil, false
	}
	return item, true
}

// handleThumbnail streams a media item's generated thumbnail. Falls back to
// 404 for items without one (e.g. HEIC photos without a pure-Go decoder),
// letting the frontend show a generic placeholder instead.
func (s *Server) handleThumbnail(w http.ResponseWriter, r *http.Request) {
	item, ok := s.lookupActiveMedia(w, r)
	if !ok {
		return
	}
	if !item.HasThumbnail {
		writeError(w, http.StatusNotFound, "no thumbnail available")
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeFile(w, r, s.processor.ThumbnailPath(item.ID))
}

// handleMediaFile streams the full-resolution original inline (used by the
// lightbox for images and as the <video> source), supporting HTTP range
// requests for video seeking.
func (s *Server) handleMediaFile(w http.ResponseWriter, r *http.Request) {
	item, ok := s.lookupActiveMedia(w, r)
	if !ok {
		return
	}
	path := s.processor.OriginalPath(item.StoredFilename)
	f, err := os.Open(path)
	if err != nil {
		writeError(w, http.StatusNotFound, "media file not found")
		return
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read media file")
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("Content-Type", item.MimeType)
	http.ServeContent(w, r, item.OriginalFilename, stat.ModTime(), f)
}

// handleMediaDownload streams the original file as an attachment so guests
// can save an individual full-resolution photo or video.
func (s *Server) handleMediaDownload(w http.ResponseWriter, r *http.Request) {
	item, ok := s.lookupActiveMedia(w, r)
	if !ok {
		return
	}
	path := s.processor.OriginalPath(item.StoredFilename)
	f, err := os.Open(path)
	if err != nil {
		writeError(w, http.StatusNotFound, "media file not found")
		return
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read media file")
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, sanitizeFilename(item.OriginalFilename)))
	w.Header().Set("Content-Type", item.MimeType)
	http.ServeContent(w, r, item.OriginalFilename, stat.ModTime(), f)
}

type likeResponse struct {
	LikeCount     int  `json:"likeCount"`
	LikedByDevice bool `json:"likedByDevice"`
}

// handleLike registers a like from the requesting device for a media item.
// Idempotent per device (a device liking twice has no additional effect).
func (s *Server) handleLike(w http.ResponseWriter, r *http.Request) {
	deviceID := deviceIDFromRequest(r)
	if deviceID == "" {
		writeError(w, http.StatusBadRequest, "missing device id")
		return
	}
	id := chi.URLParam(r, "id")
	if _, err := s.store.GetByID(r.Context(), id, deviceID); errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "media not found")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load media")
		return
	}
	if _, err := s.store.AddLike(r.Context(), id, deviceID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to like media")
		return
	}
	item, err := s.store.GetByID(r.Context(), id, deviceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load media")
		return
	}
	writeJSON(w, http.StatusOK, likeResponse{LikeCount: item.LikeCount, LikedByDevice: item.LikedByDevice})
}

// handleUnlike removes the requesting device's like from a media item.
func (s *Server) handleUnlike(w http.ResponseWriter, r *http.Request) {
	deviceID := deviceIDFromRequest(r)
	if deviceID == "" {
		writeError(w, http.StatusBadRequest, "missing device id")
		return
	}
	id := chi.URLParam(r, "id")
	if _, err := s.store.RemoveLike(r.Context(), id, deviceID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to unlike media")
		return
	}
	item, err := s.store.GetByID(r.Context(), id, deviceID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "media not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load media")
		return
	}
	writeJSON(w, http.StatusOK, likeResponse{LikeCount: item.LikeCount, LikedByDevice: item.LikedByDevice})
}
