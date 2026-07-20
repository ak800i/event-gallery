import { useMemo } from 'react'
import LightboxCore, { type Slide } from 'yet-another-react-lightbox'
import Video from 'yet-another-react-lightbox/plugins/video'
import { Download } from 'lucide-react'

import 'yet-another-react-lightbox/styles.css'

import { mediaDownloadUrl, mediaFileUrl, mediaThumbnailUrl } from '../api/client'
import type { MediaItem } from '../types'
import { LikeButton } from './LikeButton'

interface LightboxProps {
  items: MediaItem[]
  index: number
  onClose: () => void
  onIndexChange: (index: number) => void
}

type GallerySlide = Slide & { mediaItem: MediaItem }

function toSlide(item: MediaItem): GallerySlide {
  const width = item.width || 1600
  const height = item.height || 1200

  if (item.kind === 'video') {
    return {
      type: 'video',
      mediaItem: item,
      width,
      height,
      poster: item.hasThumbnail ? mediaThumbnailUrl(item.id) : undefined,
      sources: [{ src: mediaFileUrl(item.id), type: item.mimeType }],
      controls: true,
      playsInline: true,
      preload: 'metadata',
    }
  }

  return {
    type: 'image',
    mediaItem: item,
    src: mediaFileUrl(item.id),
    alt: item.originalFilename,
    width,
    height,
  }
}

/**
 * Full-viewport mixed-media viewer backed by Yet Another React Lightbox.
 * The library owns swipe/drag, keyboard, focus, preloading, and video slide
 * behavior; this wrapper only supplies wedding-specific metadata/actions.
 */
export function Lightbox({ items, index, onClose, onIndexChange }: LightboxProps) {
  const slides = useMemo(() => items.map(toSlide), [items])

  return (
    <LightboxCore
      open
      close={onClose}
      index={index}
      slides={slides}
      plugins={[Video]}
      className="wedding-lightbox"
      carousel={{ finite: true, padding: 0, spacing: '8%', preload: 1 }}
      controller={{ aria: true, closeOnBackdropClick: true }}
      video={{ controls: true, playsInline: true, preload: 'metadata' }}
      on={{ view: ({ index: currentIndex }) => currentIndex !== index && onIndexChange(currentIndex) }}
      render={{
        slideFooter: ({ slide }) => {
          const item = (slide as GallerySlide).mediaItem
          return (
            <div className={`lightbox-footer${item.kind === 'video' ? ' lightbox-footer-video' : ''}`}>
              <span className="lightbox-uploader">{item.uploaderName || 'Anonymous guest'}</span>
              <div className="lightbox-actions">
                <LikeButton
                  mediaId={item.id}
                  initialLikeCount={item.likeCount}
                  initialLiked={item.likedByDevice}
                  contextLabel={item.originalFilename}
                />
                <a
                  href={mediaDownloadUrl(item.id)}
                  download
                  className="lightbox-download"
                  aria-label={`Download original ${item.originalFilename}`}
                >
                  <Download size={20} strokeWidth={1.8} aria-hidden="true" />
                  <span>Original</span>
                </a>
              </div>
            </div>
          )
        },
      }}
    />
  )
}
