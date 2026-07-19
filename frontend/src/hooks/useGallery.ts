import { useCallback, useEffect, useRef, useState } from 'react'
import { fetchGallery } from '../api/client'
import type { GallerySort, MediaItem, SortOrder } from '../types'

interface UseGalleryResult {
  items: MediaItem[]
  loading: boolean
  error: string | null
  hasMore: boolean
  loadMore: () => void
  reload: () => void
}

/** Manages cursor-based infinite-scroll pagination of the public gallery
 * feed for a given sort/order, refetching from scratch when they change. */
export function useGallery(sort: GallerySort, order: SortOrder): UseGalleryResult {
  const [items, setItems] = useState<MediaItem[]>([])
  const [cursor, setCursor] = useState<string | undefined>(undefined)
  const [hasMore, setHasMore] = useState(true)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const requestIdRef = useRef(0)

  const loadPage = useCallback(
    async (reset: boolean) => {
      const requestId = ++requestIdRef.current
      setLoading(true)
      setError(null)
      try {
        const resp = await fetchGallery({ sort, order, cursor: reset ? undefined : cursor, limit: 30 })
        if (requestId !== requestIdRef.current) return // a newer request superseded this one
        setItems((prev) => (reset ? resp.items : [...prev, ...resp.items]))
        setCursor(resp.nextCursor)
        setHasMore(Boolean(resp.nextCursor))
      } catch {
        if (requestId !== requestIdRef.current) return
        setError('Failed to load the gallery. Please try again.')
      } finally {
        if (requestId === requestIdRef.current) setLoading(false)
      }
    },
    [sort, order, cursor],
  )

  useEffect(() => {
    void loadPage(true)
    // Only reset+reload when sort/order change, not on every cursor update.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [sort, order])

  const loadMore = useCallback(() => {
    if (!loading && hasMore) void loadPage(false)
  }, [loading, hasMore, loadPage])

  const reload = useCallback(() => {
    void loadPage(true)
  }, [loadPage])

  return { items, loading, error, hasMore, loadMore, reload }
}
