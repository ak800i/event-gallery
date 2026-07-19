import { useState } from 'react'
import type { FormEvent } from 'react'

interface GuestNameEditorProps {
  guestName: string
  onSave: (name: string) => void
  maxLength: number
}

/**
 * Small always-visible control for setting/editing the guest's public
 * display name. The name is remembered (see useGuestName) and attached to
 * every upload and shown next to it in the gallery.
 */
export function GuestNameEditor({ guestName, onSave, maxLength }: GuestNameEditorProps) {
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
          Posting as <strong>{guestName || 'Anonymous guest'}</strong>
        </span>
        <button type="button" className="link-button" onClick={() => setEditing(true)}>
          change
        </button>
      </div>
    )
  }

  return (
    <form className="guest-name-form" onSubmit={handleSubmit}>
      <label htmlFor="guest-name-input">Your name (shown next to your uploads)</label>
      <div className="guest-name-row">
        <input
          id="guest-name-input"
          type="text"
          value={draft}
          maxLength={maxLength}
          placeholder="e.g. Jamie from the bride's side"
          onChange={(e) => setDraft(e.target.value)}
          autoFocus
        />
        <button type="submit">Save</button>
      </div>
    </form>
  )
}
