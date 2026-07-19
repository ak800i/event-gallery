import { useEffect, useRef, useState } from 'react'
import { useGallery } from '../hooks/useGallery'
import type { GallerySort, MediaItem } from '../types'
import { MediaCard } from './MediaCard'
import { Lightbox } from './Lightbox'

/** The main public gallery: a responsive grid of thumbnails with
 * cursor-based infinite scroll, sortable by upload or capture time, and a
 * lightbox for full-size viewing with keyboard navigation. */
export function Gallery() {
  const [sort, setSort] = useState<GallerySort>('uploaded')
  const { items, loading, error, hasMore, loadMore } = useGallery(sort, 'desc')
  const [openIndex, setOpenIndex] = useState<number | null>(null)
  const sentinelRef = useRef<HTMLDivElement | null>(null)

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

  const openItem: MediaItem | null = openIndex !== null ? (items[openIndex] ?? null) : null

  return (
    <div className="gallery">
      <div className="gallery-controls">
        <label htmlFor="gallery-sort">Sort by</label>
        <select id="gallery-sort" value={sort} onChange={(e) => setSort(e.target.value as GallerySort)}>
          <option value="uploaded">Upload time</option>
          <option value="captured">Capture time</option>
        </select>
      </div>

      {items.length === 0 && !loading && !error && <p className="gallery-empty">No photos or videos yet -- be the first to upload!</p>}
      {error && <p className="gallery-error">{error}</p>}

      <div className="gallery-grid">
        {items.map((item, index) => (
          <MediaCard key={item.id} item={item} onOpen={() => setOpenIndex(index)} />
        ))}
      </div>

      {loading && <p className="gallery-loading">Loading...</p>}
      <div ref={sentinelRef} className="gallery-sentinel" aria-hidden="true" />
      {!hasMore && items.length > 0 && <p className="gallery-end">You've reached the end.</p>}

      {openItem && (
        <Lightbox
          item={openItem}
          onClose={() => setOpenIndex(null)}
          onPrev={openIndex! > 0 ? () => setOpenIndex((i) => (i !== null ? i - 1 : i)) : undefined}
          onNext={openIndex! < items.length - 1 ? () => setOpenIndex((i) => (i !== null ? i + 1 : i)) : undefined}
        />
      )}
    </div>
  )
}
