import { useEffect, useState, type FormEvent } from 'react'
import { CloudUpload, RotateCcw, Save } from 'lucide-react'
import { adminGetBranding, adminResetBranding, adminUpdateBranding } from '../api/client'
import type { BrandingConfig } from '../types'
import { brandingThemeStyle, formatBrandingText } from '../utils/branding'

type BrandingKey = keyof BrandingConfig

interface TextFieldDefinition {
  key: BrandingKey
  label: string
  group: 'Header' | 'Guest identity' | 'Uploads' | 'Gallery'
  multiline?: boolean
  required?: boolean
  hint?: string
}

const textFields: TextFieldDefinition[] = [
  { key: 'pageTitle', label: 'Page title', group: 'Header' },
  { key: 'pageSubtitle', label: 'Page subtitle', group: 'Header', multiline: true },
  { key: 'postingAsText', label: '“Posting as” text', group: 'Guest identity' },
  { key: 'anonymousGuestText', label: 'Anonymous guest text', group: 'Guest identity' },
  { key: 'changeNameText', label: 'Change-name button', group: 'Guest identity', required: true },
  { key: 'guestNameLabel', label: 'Name input label', group: 'Guest identity' },
  { key: 'guestNamePlaceholder', label: 'Name input placeholder', group: 'Guest identity' },
  { key: 'saveNameText', label: 'Save-name button', group: 'Guest identity', required: true },
  { key: 'uploadButtonText', label: 'Upload button', group: 'Uploads', required: true },
  {
    key: 'uploadHelperText',
    label: 'Upload helper',
    group: 'Uploads',
    multiline: true,
    hint: 'Use {maxSize} where the current file limit should appear.',
  },
  {
    key: 'uploadAwaitingApprovalText',
    label: 'Awaiting-approval confirmation',
    group: 'Uploads',
    multiline: true,
  },
  { key: 'uploadsClosedText', label: 'Uploads-closed message', group: 'Uploads', multiline: true },
  { key: 'emptyGalleryText', label: 'Empty-gallery message', group: 'Gallery', multiline: true },
  { key: 'galleryLoadingText', label: 'Gallery loading message', group: 'Gallery' },
  { key: 'galleryErrorText', label: 'Gallery error message', group: 'Gallery' },
  { key: 'galleryEndText', label: 'End-of-gallery message', group: 'Gallery' },
  { key: 'sortLabelText', label: 'Sort control label', group: 'Gallery', required: true },
  { key: 'sortUploadTimeText', label: 'Upload-time sort option', group: 'Gallery', required: true },
  { key: 'sortCaptureTimeText', label: 'Capture-time sort option', group: 'Gallery', required: true },
  { key: 'downloadOriginalText', label: 'Lightbox download text', group: 'Gallery', required: true },
]

const colorFields: Array<{ key: BrandingKey; label: string }> = [
  { key: 'backgroundColor', label: 'Page background' },
  { key: 'surfaceColor', label: 'Cards and surfaces' },
  { key: 'primaryColor', label: 'Primary accent' },
  { key: 'primaryDarkColor', label: 'Headings / dark accent' },
  { key: 'textColor', label: 'Main text' },
  { key: 'mutedColor', label: 'Muted text' },
  { key: 'borderColor', label: 'Borders' },
  { key: 'dangerColor', label: 'Errors / danger' },
]

const groups: TextFieldDefinition['group'][] = ['Header', 'Guest identity', 'Uploads', 'Gallery']

export function AdminBrandingEditor() {
  const [branding, setBranding] = useState<BrandingConfig | null>(null)
  const [saving, setSaving] = useState(false)
  const [message, setMessage] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    adminGetBranding().then(setBranding).catch(() => setError('Failed to load main-page customization.'))
  }, [])

  function update(key: BrandingKey, value: string) {
    setBranding((current) => (current ? { ...current, [key]: value } : current))
    setMessage(null)
    setError(null)
  }

  async function handleSave(event: FormEvent) {
    event.preventDefault()
    if (!branding) return
    setSaving(true)
    setMessage(null)
    setError(null)
    try {
      setBranding(await adminUpdateBranding(branding))
      setMessage('Main-page customization saved. Refresh the gallery to see it live.')
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : 'Failed to save main-page customization.')
    } finally {
      setSaving(false)
    }
  }

  async function handleReset() {
    if (!window.confirm('Reset all main-page text and colors to their defaults?')) return
    setSaving(true)
    setMessage(null)
    setError(null)
    try {
      setBranding(await adminResetBranding())
      setMessage('Main-page customization reset to defaults.')
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : 'Failed to reset main-page customization.')
    } finally {
      setSaving(false)
    }
  }

  if (error && !branding) return <p className="form-error" role="alert">{error}</p>
  if (!branding) return <p>Loading main-page customization...</p>

  const previewHelper = formatBrandingText(branding.uploadHelperText, { maxSize: '5 GB' })

  return (
    <section className="admin-branding">
      <div className="admin-branding-heading">
        <div>
          <h2>Main page</h2>
          <p>Customize all listed main-page text and choose any colors. Text is displayed literally—HTML and CSS are not interpreted.</p>
        </div>
      </div>

      <form onSubmit={handleSave} className="admin-branding-layout">
        <div className="admin-branding-editor">
          {groups.map((group) => (
            <fieldset key={group} className="admin-branding-fieldset" disabled={saving}>
              <legend>{group}</legend>
              {textFields
                .filter((field) => field.group === group)
                .map((field) => (
                  <label key={field.key} className="admin-branding-text-field">
                    <span>{field.label}{field.required ? ' *' : ''}</span>
                    {field.multiline ? (
                      <textarea
                        value={branding[field.key]}
                        onChange={(event) => update(field.key, event.target.value)}
                        rows={3}
                        required={field.required}
                      />
                    ) : (
                      <input
                        type="text"
                        value={branding[field.key]}
                        onChange={(event) => update(field.key, event.target.value)}
                        required={field.required}
                      />
                    )}
                    {field.hint && <small>{field.hint}</small>}
                  </label>
                ))}
            </fieldset>
          ))}

          <fieldset className="admin-branding-fieldset" disabled={saving}>
            <legend>Colors</legend>
            <div className="admin-color-grid">
              {colorFields.map((field) => (
                <label key={field.key} className="admin-color-field">
                  <span>{field.label}</span>
                  <span className="admin-color-inputs">
                    <input
                      type="color"
                      value={branding[field.key]}
                      onChange={(event) => update(field.key, event.target.value)}
                      aria-label={`${field.label} color picker`}
                      required
                    />
                    <input
                      type="text"
                      value={branding[field.key]}
                      onChange={(event) => update(field.key, event.target.value)}
                      pattern="#[0-9a-fA-F]{6}"
                      maxLength={7}
                      aria-label={`${field.label} hex color`}
                      required
                    />
                  </span>
                </label>
              ))}
            </div>
            <p className="admin-color-note">Color choices are unrestricted. Check the preview for readable text contrast before saving.</p>
          </fieldset>

          <div className="admin-branding-actions">
            <button type="submit" disabled={saving}>
              <Save size={17} aria-hidden="true" />
              {saving ? 'Saving...' : 'Save customization'}
            </button>
            <button type="button" onClick={handleReset} disabled={saving}>
              <RotateCcw size={17} aria-hidden="true" />
              Reset defaults
            </button>
          </div>
          {message && <p className="form-message" role="status">{message}</p>}
          {error && <p className="form-error" role="alert">{error}</p>}
        </div>

        <aside className="branding-preview" style={brandingThemeStyle(branding)} aria-label="Live main-page preview">
          <span className="branding-preview-label">Live preview</span>
          <div className="branding-preview-page">
            {branding.pageTitle && <h3>{branding.pageTitle}</h3>}
            {branding.pageSubtitle && <p>{branding.pageSubtitle}</p>}
            <div className="branding-preview-upload">
              <span aria-hidden="true">
                <CloudUpload size={20} />
              </span>
              <div>
                {branding.uploadButtonText && <strong>{branding.uploadButtonText}</strong>}
                {previewHelper && <small>{previewHelper}</small>}
              </div>
            </div>
            <div className="branding-preview-gallery">
              <span />
              <span />
              <span />
            </div>
            {branding.emptyGalleryText && <p className="branding-preview-muted">{branding.emptyGalleryText}</p>}
          </div>
        </aside>
      </form>
    </section>
  )
}
