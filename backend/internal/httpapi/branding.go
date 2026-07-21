package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"wedding-gallery/backend/internal/models"
	"wedding-gallery/backend/internal/store"
)

// brandingConfig is the admin-editable presentation contract for the public
// gallery. React renders every string as plain text; arbitrary HTML and CSS are
// intentionally not supported.
type brandingConfig struct {
	PageTitle              string `json:"pageTitle"`
	PageSubtitle           string `json:"pageSubtitle"`
	PostingAsText          string `json:"postingAsText"`
	AnonymousGuestText     string `json:"anonymousGuestText"`
	ChangeNameText         string `json:"changeNameText"`
	GuestNameLabel         string `json:"guestNameLabel"`
	GuestNamePlaceholder   string `json:"guestNamePlaceholder"`
	SaveNameText           string `json:"saveNameText"`
	UploadButtonText       string `json:"uploadButtonText"`
	UploadHelperText       string `json:"uploadHelperText"`
	UploadAwaitingApprovalText string `json:"uploadAwaitingApprovalText"`
	UploadsClosedText      string `json:"uploadsClosedText"`
	EmptyGalleryText       string `json:"emptyGalleryText"`
	GalleryLoadingText     string `json:"galleryLoadingText"`
	GalleryErrorText       string `json:"galleryErrorText"`
	GalleryEndText         string `json:"galleryEndText"`
	SortLabelText          string `json:"sortLabelText"`
	SortUploadTimeText     string `json:"sortUploadTimeText"`
	SortCaptureTimeText    string `json:"sortCaptureTimeText"`
	DownloadOriginalText   string `json:"downloadOriginalText"`
	BackgroundColor        string `json:"backgroundColor"`
	SurfaceColor           string `json:"surfaceColor"`
	PrimaryColor           string `json:"primaryColor"`
	PrimaryDarkColor       string `json:"primaryDarkColor"`
	TextColor              string `json:"textColor"`
	MutedColor             string `json:"mutedColor"`
	BorderColor            string `json:"borderColor"`
	DangerColor            string `json:"dangerColor"`
}

func defaultBrandingConfig() brandingConfig {
	return brandingConfig{
		PageTitle:            "Our Wedding Gallery",
		PageSubtitle:         "Share your photos and videos from the day -- thank you for celebrating with us!",
		PostingAsText:        "Posting as",
		AnonymousGuestText:   "Anonymous guest",
		ChangeNameText:       "change",
		GuestNameLabel:       "Your name (shown next to your uploads)",
		GuestNamePlaceholder: "e.g. Jamie from the bride's side",
		SaveNameText:         "Save",
		UploadButtonText:     "Add photos & videos",
		UploadHelperText:     "Up to {maxSize} per file · uploads resume automatically",
		UploadAwaitingApprovalText: "Upload complete. Your media is waiting for admin approval.",
		UploadsClosedText:    "Uploads are closed for this gallery.",
		EmptyGalleryText:     "No photos or videos yet -- be the first to upload!",
		GalleryLoadingText:   "Loading...",
		GalleryErrorText:     "Failed to load the gallery.",
		GalleryEndText:       "You've reached the end.",
		SortLabelText:        "Sort by",
		SortUploadTimeText:   "Upload time",
		SortCaptureTimeText:  "Capture time",
		DownloadOriginalText: "Original",
		BackgroundColor:      "#faf6f2",
		SurfaceColor:         "#ffffff",
		PrimaryColor:         "#7a5c48",
		PrimaryDarkColor:     "#5c4433",
		TextColor:            "#2b2420",
		MutedColor:           "#786f66",
		BorderColor:          "#e5ddd3",
		DangerColor:          "#b3432b",
	}
}

var hexColorPattern = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func normalizeBranding(value brandingConfig) brandingConfig {
	value.PageTitle = strings.TrimSpace(value.PageTitle)
	value.PageSubtitle = strings.TrimSpace(value.PageSubtitle)
	value.PostingAsText = strings.TrimSpace(value.PostingAsText)
	value.AnonymousGuestText = strings.TrimSpace(value.AnonymousGuestText)
	value.ChangeNameText = strings.TrimSpace(value.ChangeNameText)
	value.GuestNameLabel = strings.TrimSpace(value.GuestNameLabel)
	value.GuestNamePlaceholder = strings.TrimSpace(value.GuestNamePlaceholder)
	value.SaveNameText = strings.TrimSpace(value.SaveNameText)
	value.UploadButtonText = strings.TrimSpace(value.UploadButtonText)
	value.UploadHelperText = strings.TrimSpace(value.UploadHelperText)
	value.UploadAwaitingApprovalText = strings.TrimSpace(value.UploadAwaitingApprovalText)
	value.UploadsClosedText = strings.TrimSpace(value.UploadsClosedText)
	value.EmptyGalleryText = strings.TrimSpace(value.EmptyGalleryText)
	value.GalleryLoadingText = strings.TrimSpace(value.GalleryLoadingText)
	value.GalleryErrorText = strings.TrimSpace(value.GalleryErrorText)
	value.GalleryEndText = strings.TrimSpace(value.GalleryEndText)
	value.SortLabelText = strings.TrimSpace(value.SortLabelText)
	value.SortUploadTimeText = strings.TrimSpace(value.SortUploadTimeText)
	value.SortCaptureTimeText = strings.TrimSpace(value.SortCaptureTimeText)
	value.DownloadOriginalText = strings.TrimSpace(value.DownloadOriginalText)
	value.BackgroundColor = strings.ToLower(strings.TrimSpace(value.BackgroundColor))
	value.SurfaceColor = strings.ToLower(strings.TrimSpace(value.SurfaceColor))
	value.PrimaryColor = strings.ToLower(strings.TrimSpace(value.PrimaryColor))
	value.PrimaryDarkColor = strings.ToLower(strings.TrimSpace(value.PrimaryDarkColor))
	value.TextColor = strings.ToLower(strings.TrimSpace(value.TextColor))
	value.MutedColor = strings.ToLower(strings.TrimSpace(value.MutedColor))
	value.BorderColor = strings.ToLower(strings.TrimSpace(value.BorderColor))
	value.DangerColor = strings.ToLower(strings.TrimSpace(value.DangerColor))
	return value
}

func validateBranding(value brandingConfig) error {
	textFields := []struct {
		name     string
		value    string
		maxRunes int
		required bool
	}{
		{"pageTitle", value.PageTitle, 160, false},
		{"pageSubtitle", value.PageSubtitle, 600, false},
		{"postingAsText", value.PostingAsText, 120, false},
		{"anonymousGuestText", value.AnonymousGuestText, 120, false},
		{"changeNameText", value.ChangeNameText, 80, true},
		{"guestNameLabel", value.GuestNameLabel, 240, false},
		{"guestNamePlaceholder", value.GuestNamePlaceholder, 240, false},
		{"saveNameText", value.SaveNameText, 80, true},
		{"uploadButtonText", value.UploadButtonText, 160, true},
		{"uploadHelperText", value.UploadHelperText, 400, false},
		{"uploadAwaitingApprovalText", value.UploadAwaitingApprovalText, 400, false},
		{"uploadsClosedText", value.UploadsClosedText, 400, false},
		{"emptyGalleryText", value.EmptyGalleryText, 400, false},
		{"galleryLoadingText", value.GalleryLoadingText, 160, false},
		{"galleryErrorText", value.GalleryErrorText, 300, false},
		{"galleryEndText", value.GalleryEndText, 240, false},
		{"sortLabelText", value.SortLabelText, 120, true},
		{"sortUploadTimeText", value.SortUploadTimeText, 120, true},
		{"sortCaptureTimeText", value.SortCaptureTimeText, 120, true},
		{"downloadOriginalText", value.DownloadOriginalText, 120, true},
	}
	for _, field := range textFields {
		length := utf8.RuneCountInString(field.value)
		if field.required && length == 0 {
			return fmt.Errorf("%s must not be empty", field.name)
		}
		if length > field.maxRunes {
			return fmt.Errorf("%s must be at most %d characters", field.name, field.maxRunes)
		}
	}

	colors := []struct {
		name  string
		value string
	}{
		{"backgroundColor", value.BackgroundColor},
		{"surfaceColor", value.SurfaceColor},
		{"primaryColor", value.PrimaryColor},
		{"primaryDarkColor", value.PrimaryDarkColor},
		{"textColor", value.TextColor},
		{"mutedColor", value.MutedColor},
		{"borderColor", value.BorderColor},
		{"dangerColor", value.DangerColor},
	}
	for _, color := range colors {
		if !hexColorPattern.MatchString(color.value) {
			return fmt.Errorf("%s must be a #RRGGBB color", color.name)
		}
	}
	return nil
}

func (s *Server) loadBranding(ctx context.Context) (brandingConfig, error) {
	value := defaultBrandingConfig()
	raw, ok, err := s.store.GetConfig(ctx, store.ConfigKeyBranding)
	if err != nil {
		return brandingConfig{}, err
	}
	if !ok || strings.TrimSpace(raw) == "" {
		return value, nil
	}
	// Unmarshal into defaults so fields added by later releases retain their
	// default when reading JSON written by an older release.
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		slog.Warn("invalid stored branding; using defaults", "error", err)
		return defaultBrandingConfig(), nil
	}
	value = normalizeBranding(value)
	if err := validateBranding(value); err != nil {
		slog.Warn("invalid stored branding; using defaults", "error", err)
		return defaultBrandingConfig(), nil
	}
	return value, nil
}

func (s *Server) saveBranding(ctx context.Context, value brandingConfig) (brandingConfig, error) {
	value = normalizeBranding(value)
	if err := validateBranding(value); err != nil {
		return brandingConfig{}, err
	}
	if value == defaultBrandingConfig() {
		if err := s.store.DeleteConfig(ctx, store.ConfigKeyBranding); err != nil {
			return brandingConfig{}, err
		}
		return value, nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return brandingConfig{}, fmt.Errorf("encode branding configuration: %w", err)
	}
	if err := s.store.SetConfig(ctx, store.ConfigKeyBranding, string(raw)); err != nil {
		return brandingConfig{}, err
	}
	return value, nil
}

func (s *Server) handleAdminGetBranding(w http.ResponseWriter, r *http.Request) {
	value, err := s.loadBranding(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load branding")
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func (s *Server) handleAdminUpdateBranding(w http.ResponseWriter, r *http.Request) {
	var req brandingConfig
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req = normalizeBranding(req)
	if err := validateBranding(req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	value, err := s.saveBranding(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save branding")
		return
	}
	_ = s.store.RecordAudit(r.Context(), models.ActionConfig, "admin", "", "", "gallery branding updated")
	writeJSON(w, http.StatusOK, value)
}

func (s *Server) handleAdminResetBranding(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteConfig(r.Context(), store.ConfigKeyBranding); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to reset branding")
		return
	}
	_ = s.store.RecordAudit(r.Context(), models.ActionConfig, "admin", "", "", "gallery branding reset to defaults")
	writeJSON(w, http.StatusOK, defaultBrandingConfig())
}
