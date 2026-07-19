import { useEffect, useState } from 'react'
import {
  adminAuditLog,
  adminBulkDelete,
  adminBulkRestore,
  adminGetConfig,
  adminListMedia,
  adminLogout,
  adminUpdateConfig,
  mediaThumbnailUrl,
} from '../api/client'
import type { AuditEntry, MediaItem, MediaStatus } from '../types'

type Tab = 'active' | 'trashed' | 'audit' | 'settings'

interface AdminDashboardProps {
  onLoggedOut: () => void
}

export function AdminDashboard({ onLoggedOut }: AdminDashboardProps) {
  const [tab, setTab] = useState<Tab>('active')

  async function handleLogout() {
    await adminLogout()
    onLoggedOut()
  }

  return (
    <div className="admin-dashboard">
      <header className="admin-header">
        <h1>Gallery admin</h1>
        <button type="button" onClick={handleLogout}>
          Log out
        </button>
      </header>
      <nav className="admin-tabs">
        <button type="button" className={tab === 'active' ? 'active' : ''} onClick={() => setTab('active')}>
          All media
        </button>
        <button type="button" className={tab === 'trashed' ? 'active' : ''} onClick={() => setTab('trashed')}>
          Trash
        </button>
        <button type="button" className={tab === 'audit' ? 'active' : ''} onClick={() => setTab('audit')}>
          Audit log
        </button>
        <button type="button" className={tab === 'settings' ? 'active' : ''} onClick={() => setTab('settings')}>
          Settings
        </button>
      </nav>

      {tab === 'active' && <AdminMediaList status="active" />}
      {tab === 'trashed' && <AdminMediaList status="trashed" />}
      {tab === 'audit' && <AdminAuditLog />}
      {tab === 'settings' && <AdminSettings />}
    </div>
  )
}

function AdminMediaList({ status }: { status: MediaStatus }) {
  const [items, setItems] = useState<MediaItem[]>([])
  const [cursor, setCursor] = useState<string | undefined>(undefined)
  const [hasMore, setHasMore] = useState(true)
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [loading, setLoading] = useState(false)

  async function load(reset: boolean) {
    setLoading(true)
    if (reset) setSelected(new Set())
    try {
      const resp = await adminListMedia({ status, cursor: reset ? undefined : cursor, limit: 60 })
      setItems((prev) => (reset ? resp.items : [...prev, ...resp.items]))
      setCursor(resp.nextCursor)
      setHasMore(Boolean(resp.nextCursor))
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    void load(true)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [status])

  function toggle(id: string) {
    setSelected((prev) => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }

  function selectAll() {
    setSelected(new Set(items.map((i) => i.id)))
  }

  function clearSelection() {
    setSelected(new Set())
  }

  async function handleDelete() {
    if (selected.size === 0) return
    await adminBulkDelete(Array.from(selected))
    setItems((prev) => prev.filter((i) => !selected.has(i.id)))
    clearSelection()
  }

  async function handleRestore() {
    if (selected.size === 0) return
    await adminBulkRestore(Array.from(selected))
    setItems((prev) => prev.filter((i) => !selected.has(i.id)))
    clearSelection()
  }

  return (
    <div className="admin-media-list">
      <div className="admin-toolbar">
        <button type="button" onClick={selectAll} disabled={items.length === 0}>
          Select all
        </button>
        <button type="button" onClick={clearSelection} disabled={selected.size === 0}>
          Clear selection
        </button>
        <span className="admin-selection-count">{selected.size} selected</span>
        {status === 'active' ? (
          <button type="button" className="danger" onClick={handleDelete} disabled={selected.size === 0}>
            Move to trash
          </button>
        ) : (
          <button type="button" onClick={handleRestore} disabled={selected.size === 0}>
            Restore
          </button>
        )}
      </div>

      <div className="admin-media-grid">
        {items.map((item) => (
          <label key={item.id} className={`admin-media-tile${selected.has(item.id) ? ' selected' : ''}`}>
            <input type="checkbox" checked={selected.has(item.id)} onChange={() => toggle(item.id)} />
            {item.hasThumbnail ? (
              <img src={mediaThumbnailUrl(item.id)} alt="" loading="lazy" />
            ) : (
              <div className="media-card-placeholder">{item.kind === 'video' ? '\u{1f3a5}' : '\u{1f5bc}\ufe0f'}</div>
            )}
            <span className="admin-media-filename">{item.originalFilename}</span>
          </label>
        ))}
      </div>

      {items.length === 0 && !loading && <p>No items.</p>}
      {hasMore && (
        <button type="button" onClick={() => load(false)} disabled={loading}>
          {loading ? 'Loading...' : 'Load more'}
        </button>
      )}
    </div>
  )
}

function AdminAuditLog() {
  const [entries, setEntries] = useState<AuditEntry[]>([])
  const [cursor, setCursor] = useState<string | undefined>(undefined)
  const [hasMore, setHasMore] = useState(true)
  const [loading, setLoading] = useState(false)

  async function load(reset: boolean) {
    setLoading(true)
    try {
      const resp = await adminAuditLog(reset ? undefined : cursor)
      setEntries((prev) => (reset ? resp.entries : [...prev, ...resp.entries]))
      setCursor(resp.nextCursor)
      setHasMore(Boolean(resp.nextCursor))
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    void load(true)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  return (
    <div className="admin-audit-log">
      <table>
        <thead>
          <tr>
            <th>When</th>
            <th>Action</th>
            <th>Actor</th>
            <th>File</th>
            <th>Details</th>
          </tr>
        </thead>
        <tbody>
          {entries.map((e) => (
            <tr key={e.id}>
              <td>{new Date(e.createdAt).toLocaleString()}</td>
              <td>{e.action}</td>
              <td>{e.actor}</td>
              <td>{e.filename ?? '-'}</td>
              <td>{e.details ?? ''}</td>
            </tr>
          ))}
        </tbody>
      </table>
      {hasMore && (
        <button type="button" onClick={() => load(false)} disabled={loading}>
          {loading ? 'Loading...' : 'Load more'}
        </button>
      )}
    </div>
  )
}

function AdminSettings() {
  const [uploadExpiresAt, setUploadExpiresAt] = useState<string>('')
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [message, setMessage] = useState<string | null>(null)

  useEffect(() => {
    adminGetConfig()
      .then((cfg) => setUploadExpiresAt(cfg.uploadExpiresAt ? toLocalInputValue(cfg.uploadExpiresAt) : ''))
      .finally(() => setLoading(false))
  }, [])

  async function handleSave() {
    setSaving(true)
    setMessage(null)
    try {
      const iso = uploadExpiresAt ? new Date(uploadExpiresAt).toISOString() : null
      await adminUpdateConfig(iso)
      setMessage('Saved.')
    } catch {
      setMessage('Failed to save.')
    } finally {
      setSaving(false)
    }
  }

  async function handleClear() {
    setUploadExpiresAt('')
    setSaving(true)
    try {
      await adminUpdateConfig(null)
      setMessage('Upload expiry cleared; uploads remain open indefinitely.')
    } finally {
      setSaving(false)
    }
  }

  if (loading) return <p>Loading...</p>

  return (
    <div className="admin-settings">
      <h2>Upload window</h2>
      <p>
        After this date/time, new uploads are refused, but the gallery remains fully viewable and downloadable for
        guests indefinitely.
      </p>
      <label htmlFor="upload-expiry">Uploads close at</label>
      <input
        id="upload-expiry"
        type="datetime-local"
        value={uploadExpiresAt}
        onChange={(e) => setUploadExpiresAt(e.target.value)}
      />
      <div className="admin-settings-actions">
        <button type="button" onClick={handleSave} disabled={saving}>
          Save
        </button>
        <button type="button" onClick={handleClear} disabled={saving}>
          Clear (never close)
        </button>
      </div>
      {message && <p className="form-message">{message}</p>}
    </div>
  )
}

function toLocalInputValue(iso: string): string {
  const d = new Date(iso)
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`
}
