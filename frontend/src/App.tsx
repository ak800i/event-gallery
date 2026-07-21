import { useCallback, useEffect, useState } from 'react'
import { fetchPublicConfig } from './api/client'
import type { PublicConfig } from './types'
import { useGuestName } from './hooks/useGuestName'
import { GuestNameEditor } from './components/GuestNameEditor'
import { UploadPanel } from './components/UploadPanel'
import { Gallery } from './components/Gallery'
import { AdminApp } from './components/AdminApp'
import { brandingThemeStyle, DEFAULT_BRANDING } from './utils/branding'

/** Top-level app shell. Uses the URL path to decide between the public
 * gallery ("/") and the admin area ("/admin"), without pulling in a full
 * router dependency for what is effectively two screens. */
export function App() {
  const isAdmin = window.location.pathname.startsWith('/admin')
  if (isAdmin) return <AdminApp />
  return <GuestApp />
}

function GuestApp() {
  const [guestName, setGuestName] = useGuestName()
  const [config, setConfig] = useState<PublicConfig | null>(null)
  const [galleryKey, setGalleryKey] = useState(0)

  const loadConfig = useCallback(() => {
    fetchPublicConfig().then(setConfig).catch(() => setConfig(null))
  }, [])

  useEffect(() => {
    loadConfig()
  }, [loadConfig])

  const handleUploadComplete = useCallback(() => {
    // Force the gallery to remount and fetch fresh data from page 1 so new
    // uploads (once processed by the server) show up promptly.
    setGalleryKey((k) => k + 1)
  }, [])

  const branding = config?.branding ?? DEFAULT_BRANDING

  useEffect(() => {
    document.title = branding.pageTitle || 'Wedding Gallery'
  }, [branding.pageTitle])

  return (
    <div className="guest-page" style={brandingThemeStyle(branding)}>
      <div className="app">
        <header className="app-header">
          {branding.pageTitle && <h1>{branding.pageTitle}</h1>}
          {branding.pageSubtitle && <p className="app-subtitle">{branding.pageSubtitle}</p>}
        </header>

        <GuestNameEditor
          guestName={guestName}
          onSave={setGuestName}
          maxLength={config?.guestNameMaxLength ?? 60}
          branding={branding}
        />

        {config && (
          <section className="upload-section">
            <UploadPanel guestName={guestName} config={config} branding={branding} onUploadComplete={handleUploadComplete} />
          </section>
        )}

        <section className="gallery-section" key={galleryKey}>
          <Gallery branding={branding} />
        </section>
      </div>
    </div>
  )
}
