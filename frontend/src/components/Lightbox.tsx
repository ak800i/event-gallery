import { useEffect } from 'react'
import { mediaDownloadUrl, mediaFileUrl } from '../api/client'
import type { MediaItem } from '../types'
import { LikeButton } from './LikeButton'

interface LightboxProps {
  item: MediaItem
  onClose: () => void
  onPrev?: () => void
  onNext?: () => void
}

/** Full-screen viewer for a single photo or video, with keyboard
 * navigation (Escape to close, arrow keys to move between items). */
export function Lightbox({ item, onClose, onPrev, onNext }: LightboxProps) {
  useEffect(() => {
    function onKeyDown(e: KeyboardEvent) {
      if (e.key === 'Escape') onClose()
      if (e.key === 'ArrowLeft' && onPrev) onPrev()
      if (e.key === 'ArrowRight' && onNext) onNext()
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [onClose, onPrev, onNext])

  return (
    <div className="lightbox-backdrop" role="dialog" aria-modal="true" onClick={onClose}>
      <div className="lightbox-content" onClick={(e) => e.stopPropagation()}>
        <button type="button" className="lightbox-close" onClick={onClose} aria-label="Close">
          {'\u2715'}
        </button>
        {onPrev && (
          <button type="button" className="lightbox-nav lightbox-prev" onClick={onPrev} aria-label="Previous">
            {'\u2039'}
          </button>
        )}
        {onNext && (
          <button type="button" className="lightbox-nav lightbox-next" onClick={onNext} aria-label="Next">
            {'\u203a'}
          </button>
        )}

        <div className="lightbox-media">
          {item.kind === 'video' ? (
            <video src={mediaFileUrl(item.id)} controls autoPlay />
          ) : (
            <img src={mediaFileUrl(item.id)} alt={item.originalFilename} />
          )}
        </div>

        <div className="lightbox-footer">
          <span>{item.uploaderName || 'Anonymous guest'}</span>
          <div className="lightbox-actions">
            <LikeButton mediaId={item.id} initialLikeCount={item.likeCount} initialLiked={item.likedByDevice} />
            <a href={mediaDownloadUrl(item.id)} download className="download-link">
              Download original
            </a>
          </div>
        </div>
      </div>
    </div>
  )
}
