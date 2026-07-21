import { lazy, Suspense, useEffect, useMemo, useRef, useState, type CSSProperties } from 'react'
import { RowsPhotoAlbum, type Photo } from 'react-photo-album'
import { ArrowUpDown } from 'lucide-react'
import 'react-photo-album/rows.css'

import { useGallery } from '../hooks/useGallery'
import { mediaThumbnailUrl } from '../api/client'
import type { BrandingConfig, GallerySort, MediaItem } from '../types'
import { MediaCard } from './MediaCard'

const GalleryLightbox = lazy(() => import('./Lightbox').then(({ Lightbox }) => ({ default: Lightbox })))

interface GalleryPhoto extends Photo {
  item: MediaItem
}

interface GalleryProps {
  branding: BrandingConfig
  refreshRequest: { id: number; expectedUploads: number }
}

/** The main public gallery: a responsive, image-first timeline with
 * cursor-based infinite scroll, sorting, and a mobile-friendly mixed-media
 * lightbox backed by maintained gallery components. */
export function Gallery({ branding, refreshRequest }: GalleryProps) {
  const [sort, setSort] = useState<GallerySort>('uploaded')
  const { items, loading, error, hasMore, loadMore, refreshNewest } = useGallery(sort, 'desc')
  const [openIndex, setOpenIndex] = useState<number | null>(null)
  const [processingUploads, setProcessingUploads] = useState(false)
  const sentinelRef = useRef<HTMLDivElement | null>(null)

  const photos = useMemo<GalleryPhoto[]>(
    () =>
      items.map((item) => ({
        key: item.id,
        src: mediaThumbnailUrl(item.id),
        width: item.width || 1,
        height: item.height || 1,
        alt: item.originalFilename,
        label: `Open ${item.originalFilename}`,
        item,
      })),
    [items],
  )

  useEffect(() => {
    const node = sentinelRef.current
    if (!node) return
    const observer = new IntersectionObserver(
      (entries) => {
        if (entries.some((e) => e.isIntersecting)) loadMore()
      },
      { rootMargin: '400px' },
    )
    observer.observe(node)
    return () => observer.disconnect()
  }, [loadMore])

  // Keep the next page ready while someone swipes near the end of the
  // currently loaded lightbox slides, so navigation feels continuous.
  useEffect(() => {
    if (openIndex !== null && hasMore && !loading && openIndex >= items.length - 5) loadMore()
  }, [hasMore, items.length, loadMore, loading, openIndex])

  useEffect(() => {
    if (refreshRequest.id === 0 || refreshRequest.expectedUploads === 0) return
    let cancelled = false
    let timer: ReturnType<typeof setTimeout> | undefined
    let cancelWait: (() => void) | undefined
    const delays = [0, 750, 1500, 3000, 5000, 8000, 12000, 15000]

    async function pollForProcessedUploads() {
      let discovered = 0
      setProcessingUploads(true)
      for (const delay of delays) {
        if (delay > 0) {
          await new Promise<void>((resolve) => {
            cancelWait = resolve
            timer = setTimeout(resolve, delay)
          })
          cancelWait = undefined
        }
        if (cancelled) return
        try {
          discovered += await refreshNewest(refreshRequest.expectedUploads)
          if (discovered >= refreshRequest.expectedUploads) break
        } catch {
          // A later attempt may succeed; the ordinary gallery error state is
          // intentionally unaffected by this silent background refresh.
        }
      }
      if (!cancelled) setProcessingUploads(false)
    }

    void pollForProcessedUploads()
    return () => {
      cancelled = true
      if (timer) clearTimeout(timer)
      cancelWait?.()
    }
  }, [refreshNewest, refreshRequest.expectedUploads, refreshRequest.id])

  return (
    <div className="gallery">
      <div className="gallery-controls">
        {processingUploads && (
          <span className="gallery-processing" role="status">
            <span className="gallery-processing-spinner" aria-hidden="true" />
            {branding.galleryLoadingText || <span className="sr-only">Processing uploads</span>}
          </span>
        )}
        <label
          className="sort-control"
          title={sort === 'uploaded' ? branding.sortUploadTimeText : branding.sortCaptureTimeText}
        >
          <ArrowUpDown size={20} strokeWidth={1.8} aria-hidden="true" />
          <span className="sr-only">{branding.sortLabelText}</span>
          <select
            value={sort}
            onChange={(e) => setSort(e.target.value as GallerySort)}
            aria-label={branding.sortLabelText || 'Sort by'}
          >
            <option value="uploaded">{branding.sortUploadTimeText}</option>
            <option value="captured">{branding.sortCaptureTimeText}</option>
          </select>
        </label>
      </div>

      {items.length === 0 && !loading && !error && branding.emptyGalleryText && (
        <p className="gallery-empty">{branding.emptyGalleryText}</p>
      )}
      {error && branding.galleryErrorText && <p className="gallery-error">{branding.galleryErrorText}</p>}

      {photos.length > 0 && (
        <RowsPhotoAlbum
          photos={photos}
          defaultContainerWidth={1100}
          spacing={(containerWidth) => (containerWidth < 600 ? 4 : 7)}
          padding={0}
          targetRowHeight={(containerWidth) => (containerWidth < 600 ? 300 : containerWidth < 900 ? 380 : 440)}
          rowConstraints={{ singleRowMaxHeight: 520 }}
          render={{
            photo: (_props, { photo, index, width, height }) => (
              <MediaCard
                key={photo.key}
                item={photo.item}
                anonymousGuestText={branding.anonymousGuestText}
                onOpen={() => setOpenIndex(index)}
                style={
                  {
                    '--react-photo-album--photo-width': width,
                    '--react-photo-album--photo-height': height,
                    aspectRatio: `${width} / ${height}`,
                  } as CSSProperties
                }
              />
            ),
          }}
        />
      )}

      {loading && branding.galleryLoadingText && <p className="gallery-loading">{branding.galleryLoadingText}</p>}
      <div ref={sentinelRef} className="gallery-sentinel" aria-hidden="true" />
      {!hasMore && items.length > 0 && branding.galleryEndText && <p className="gallery-end">{branding.galleryEndText}</p>}

      {openIndex !== null && items.length > 0 && (
        <Suspense fallback={null}>
          <GalleryLightbox
            items={items}
            index={openIndex}
            branding={branding}
            onClose={() => setOpenIndex(null)}
            onIndexChange={setOpenIndex}
          />
        </Suspense>
      )}
    </div>
  )
}
