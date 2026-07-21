import type { CSSProperties } from 'react'
import type { BrandingConfig } from '../types'

type ThemeStyle = CSSProperties & Record<`--color-${string}`, string>

// Immediate render fallback while /api/config/public is loading. The backend
// remains authoritative and returns these same values when no override exists.
export const DEFAULT_BRANDING: BrandingConfig = {
  pageTitle: 'Our Wedding Gallery',
  pageSubtitle: 'Share your photos and videos from the day -- thank you for celebrating with us!',
  postingAsText: 'Posting as',
  anonymousGuestText: 'Anonymous guest',
  changeNameText: 'change',
  guestNameLabel: 'Your name (shown next to your uploads)',
  guestNamePlaceholder: "e.g. Jamie from the bride's side",
  saveNameText: 'Save',
  uploadButtonText: 'Add photos & videos',
  uploadHelperText: 'Up to {maxSize} per file · uploads resume automatically',
  uploadAwaitingApprovalText: 'Upload complete. Your media is waiting for admin approval.',
  uploadsClosedText: 'Uploads are closed for this gallery.',
  emptyGalleryText: 'No photos or videos yet -- be the first to upload!',
  galleryLoadingText: 'Loading...',
  galleryErrorText: 'Failed to load the gallery.',
  galleryEndText: "You've reached the end.",
  sortLabelText: 'Sort by',
  sortUploadTimeText: 'Upload time',
  sortCaptureTimeText: 'Capture time',
  downloadOriginalText: 'Original',
  backgroundColor: '#faf6f2',
  surfaceColor: '#ffffff',
  primaryColor: '#7a5c48',
  primaryDarkColor: '#5c4433',
  textColor: '#2b2420',
  mutedColor: '#786f66',
  borderColor: '#e5ddd3',
  dangerColor: '#b3432b',
}

/** Scope admin-selected colors to the public page (or an admin preview)
 * through the app's existing CSS-variable theme contract. */
export function brandingThemeStyle(branding: BrandingConfig): ThemeStyle {
  return {
    '--color-bg': branding.backgroundColor,
    '--color-surface': branding.surfaceColor,
    '--color-primary': branding.primaryColor,
    '--color-primary-dark': branding.primaryDarkColor,
    '--color-text': branding.textColor,
    '--color-muted': branding.mutedColor,
    '--color-border': branding.borderColor,
    '--color-danger': branding.dangerColor,
  }
}

/** Replace supported plain-text placeholders without interpreting HTML. */
export function formatBrandingText(template: string, values: Record<string, string>): string {
  return Object.entries(values).reduce((text, [name, value]) => text.replaceAll(`{${name}}`, value), template)
}
