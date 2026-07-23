import { useState } from 'react'
import type { FormEvent } from 'react'
import type { BrandingConfig } from '../types'

interface GuestNameEditorProps {
  guestName: string
  onSave: (name: string) => void
  maxLength: number
  branding: BrandingConfig
}

/**
 * Small always-visible control for setting/editing the guest's public
 * display name. The name is remembered (see useGuestName) and attached to
 * every upload and shown next to it in the gallery.
 */
export function GuestNameEditor({ guestName, onSave, maxLength, branding }: GuestNameEditorProps) {
  const [editing, setEditing] = useState(guestName.trim().length === 0)
  const [draft, setDraft] = useState(guestName)

  function handleSubmit(e: FormEvent) {
    e.preventDefault()
    const trimmed = draft.trim().slice(0, maxLength)
    onSave(trimmed)
    setEditing(false)
  }

  if (!editing) {
    return (
      <div className="guest-name-display">
        <span>
          {branding.postingAsText} <strong>{guestName || branding.anonymousGuestText}</strong>
        </span>
        <button
          type="button"
          className="link-button"
          onClick={() => setEditing(true)}
          aria-label={branding.changeNameText || 'Change display name'}
        >
          {branding.changeNameText}
        </button>
      </div>
    )
  }

  return (
    <form className="guest-name-form" onSubmit={handleSubmit}>
      {branding.guestNameLabel && <label htmlFor="guest-name-input">{branding.guestNameLabel}</label>}
      <div className="guest-name-row">
        <input
          id="guest-name-input"
          type="text"
          value={draft}
          maxLength={maxLength}
          placeholder={branding.guestNamePlaceholder}
          aria-label={branding.guestNameLabel || 'Your name'}
          onChange={(e) => setDraft(e.target.value)}
          autoFocus
        />
        <button type="submit" className="btn-primary" aria-label={branding.saveNameText || 'Save name'}>
          {branding.saveNameText}
        </button>
      </div>
    </form>
  )
}
