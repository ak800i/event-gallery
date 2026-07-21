// Shared TypeScript types mirroring the backend's JSON DTOs
// (see backend/internal/httpapi/public.go and admin.go).

export type MediaKind = 'image' | 'video'
export type MediaStatus = 'active' | 'trashed'

export interface MediaItem {
  id: string
  originalFilename: string
  kind: MediaKind
  mimeType: string
  sizeBytes: number
  width?: number
  height?: number
  durationSeconds?: number
  hasThumbnail: boolean
  capturedAt?: string
  uploadedAt: string
  uploaderName: string
  likeCount: number
  likedByDevice: boolean
  status?: MediaStatus
}

export interface GalleryResponse {
  items: MediaItem[]
  nextCursor?: string
}

export interface BrandingConfig {
  pageTitle: string
  pageSubtitle: string
  postingAsText: string
  anonymousGuestText: string
  changeNameText: string
  guestNameLabel: string
  guestNamePlaceholder: string
  saveNameText: string
  uploadButtonText: string
  uploadHelperText: string
  uploadsClosedText: string
  emptyGalleryText: string
  galleryLoadingText: string
  galleryErrorText: string
  galleryEndText: string
  sortLabelText: string
  sortUploadTimeText: string
  sortCaptureTimeText: string
  downloadOriginalText: string
  backgroundColor: string
  surfaceColor: string
  primaryColor: string
  primaryDarkColor: string
  textColor: string
  mutedColor: string
  borderColor: string
  dangerColor: string
}

export interface PublicConfig {
  uploadsEnabled: boolean
  uploadExpiresAt?: string
  maxUploadBytes: number
  uploadConcurrency: number
  allowedImageMimeTypes: string[]
  allowedVideoMimeTypes: string[]
  guestNameMaxLength: number
  branding: BrandingConfig
}

export interface UploadCheckResponse {
  duplicate: boolean
  mediaId?: string
}

export interface LikeResponse {
  likeCount: number
  likedByDevice: boolean
}

export interface AuditEntry {
  id: number
  action: string
  actor: string
  mediaId?: string
  filename?: string
  details?: string
  createdAt: string
}

export interface AuditLogResponse {
  entries: AuditEntry[]
  nextCursor?: string
}

export type GallerySort = 'uploaded' | 'captured'
export type SortOrder = 'asc' | 'desc'
