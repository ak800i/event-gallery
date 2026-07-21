import { useCallback, useEffect, useRef, useState } from 'react'
import { fetchGallery } from '../api/client'
import type { GallerySort, MediaItem, SortOrder } from '../types'

interface UseGalleryResult {
  items: MediaItem[]
  loading: boolean
  error: string | null
  hasMore: boolean
  loadMore: () => void
  refreshNewest: (expectedUploads: number) => Promise<number>
}

function compareMedia(a: MediaItem, b: MediaItem, sort: GallerySort, order: SortOrder): number {
  const aTime = sort === 'captured' ? (a.capturedAt ?? a.uploadedAt) : a.uploadedAt
  const bTime = sort === 'captured' ? (b.capturedAt ?? b.uploadedAt) : b.uploadedAt
  const comparison = aTime === bTime ? a.id.localeCompare(b.id) : aTime.localeCompare(bTime)
  return order === 'asc' ? comparison : -comparison
}

function mergeUnique(current: MediaItem[], incoming: MediaItem[], sort: GallerySort, order: SortOrder): MediaItem[] {
  const byID = new Map(current.map((item) => [item.id, item]))
  for (const item of incoming) byID.set(item.id, item)
  return Array.from(byID.values()).sort((a, b) => compareMedia(a, b, sort, order))
}

/** Manages cursor-based infinite-scroll pagination of the public gallery.
 * Background refreshes merge newly processed uploads without clearing the
 * existing grid, cursor, lightbox state, or scroll position. */
export function useGallery(sort: GallerySort, order: SortOrder): UseGalleryResult {
  const [items, setItems] = useState<MediaItem[]>([])
  const [cursor, setCursor] = useState<string | undefined>(undefined)
  const [hasMore, setHasMore] = useState(true)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const requestIdRef = useRef(0)
  const itemsRef = useRef<MediaItem[]>([])

  const replaceItems = useCallback((next: MediaItem[]) => {
    itemsRef.current = next
    setItems(next)
  }, [])

  const loadPage = useCallback(
    async (reset: boolean) => {
      const requestId = ++requestIdRef.current
      setLoading(true)
      setError(null)
      try {
        const resp = await fetchGallery({ sort, order, cursor: reset ? undefined : cursor, limit: 30 })
        if (requestId !== requestIdRef.current) return // a newer request superseded this one
        const next = reset ? resp.items : mergeUnique(itemsRef.current, resp.items, sort, order)
        replaceItems(next)
        setCursor(resp.nextCursor)
        setHasMore(Boolean(resp.nextCursor))
      } catch {
        if (requestId !== requestIdRef.current) return
        setError('Failed to load the gallery. Please try again.')
      } finally {
        if (requestId === requestIdRef.current) setLoading(false)
      }
    },
    [sort, order, cursor, replaceItems],
  )

  useEffect(() => {
    void loadPage(true)
    // Only reset+reload when sort/order change, not on every cursor update.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [sort, order])

  const loadMore = useCallback(() => {
    if (!loading && hasMore) void loadPage(false)
  }, [loading, hasMore, loadPage])

  const refreshNewest = useCallback(async (expectedUploads: number) => {
    // Request enough of the current first page to contain the old visible
    // items plus the completed batch. This supports batches up to the public
    // API's 100-item cap without pulling unrelated pagination pages.
    const limit = Math.min(100, Math.max(30, itemsRef.current.length + expectedUploads))
    const resp = await fetchGallery({ sort, order, limit })
    const existingIDs = new Set(itemsRef.current.map((item) => item.id))
    const added = resp.items.filter((item) => !existingIDs.has(item.id)).length
    replaceItems(mergeUnique(itemsRef.current, resp.items, sort, order))
    return added
  }, [order, replaceItems, sort])

  return { items, loading, error, hasMore, loadMore, refreshNewest }
}
