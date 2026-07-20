import type { CSSProperties } from 'react'
import { Download, Image as ImageIcon, Play, Video as VideoIcon } from 'lucide-react'
import { mediaDownloadUrl, mediaThumbnailUrl } from '../api/client'
import type { MediaItem } from '../types'
import { LikeButton } from './LikeButton'

interface MediaCardProps {
  item: MediaItem
  onOpen: () => void
  style?: CSSProperties
}

/** One image-forward tile in the responsive gallery. Secondary metadata and
 * actions sit on a quiet gradient overlay, similar to modern photo timelines. */
export function MediaCard({ item, onOpen, style }: MediaCardProps) {
  return (
    <div className="media-card react-photo-album--photo" style={style}>
      <button type="button" className="media-card-thumb" onClick={onOpen} aria-label={`Open ${item.originalFilename}`}>
        {item.hasThumbnail ? (
          <img src={mediaThumbnailUrl(item.id)} alt="" loading="lazy" />
        ) : (
          <span className="media-card-placeholder" aria-hidden="true">
            {item.kind === 'video' ? <VideoIcon size={34} strokeWidth={1.5} /> : <ImageIcon size={34} strokeWidth={1.5} />}
          </span>
        )}
        {item.kind === 'video' && (
          <span className="video-badge" aria-hidden="true">
            <Play size={15} fill="currentColor" strokeWidth={1.5} />
          </span>
        )}
      </button>
      <div className="media-card-meta">
        <span className="uploader-name" title={item.uploaderName || 'Anonymous guest'}>
          {item.uploaderName || 'Anonymous guest'}
        </span>
        <div className="media-card-actions">
          <LikeButton
            mediaId={item.id}
            initialLikeCount={item.likeCount}
            initialLiked={item.likedByDevice}
            contextLabel={item.originalFilename}
          />
          <a
            className="download-link"
            href={mediaDownloadUrl(item.id)}
            download
            aria-label={`Download original ${item.originalFilename}`}
          >
            <Download size={18} strokeWidth={1.8} aria-hidden="true" />
          </a>
        </div>
      </div>
    </div>
  )
}
