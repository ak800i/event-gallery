import { mediaDownloadUrl, mediaThumbnailUrl } from '../api/client'
import type { MediaItem } from '../types'
import { LikeButton } from './LikeButton'

interface MediaCardProps {
  item: MediaItem
  onOpen: () => void
}

/** One tile in the gallery grid: thumbnail, kind badge, like button, and a
 * direct download link for the original file. */
export function MediaCard({ item, onOpen }: MediaCardProps) {
  return (
    <div className="media-card">
      <button type="button" className="media-card-thumb" onClick={onOpen} aria-label={`Open ${item.originalFilename}`}>
        {item.hasThumbnail ? (
          <img src={mediaThumbnailUrl(item.id)} alt="" loading="lazy" />
        ) : (
          <div className="media-card-placeholder">{item.kind === 'video' ? '\u{1f3a5}' : '\u{1f5bc}\ufe0f'}</div>
        )}
        {item.kind === 'video' && <span className="video-badge">\u25b6</span>}
      </button>
      <div className="media-card-meta">
        <span className="uploader-name" title={item.uploaderName || 'Anonymous guest'}>
          {item.uploaderName || 'Anonymous guest'}
        </span>
        <div className="media-card-actions">
          <LikeButton mediaId={item.id} initialLikeCount={item.likeCount} initialLiked={item.likedByDevice} />
          <a className="download-link" href={mediaDownloadUrl(item.id)} download aria-label="Download original">
            {'\u2b07\ufe0f'}
          </a>
        </div>
      </div>
    </div>
  )
}
