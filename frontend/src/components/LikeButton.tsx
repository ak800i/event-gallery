import { useState } from 'react'
import { likeMedia, unlikeMedia } from '../api/client'

interface LikeButtonProps {
  mediaId: string
  initialLikeCount: number
  initialLiked: boolean
}

export function LikeButton({ mediaId, initialLikeCount, initialLiked }: LikeButtonProps) {
  const [likeCount, setLikeCount] = useState(initialLikeCount)
  const [liked, setLiked] = useState(initialLiked)
  const [busy, setBusy] = useState(false)

  async function toggle() {
    if (busy) return
    setBusy(true)
    // Optimistic update for a snappy feel; reconciled with the server
    // response (or rolled back on error) below.
    const nextLiked = !liked
    setLiked(nextLiked)
    setLikeCount((c) => c + (nextLiked ? 1 : -1))
    try {
      const resp = nextLiked ? await likeMedia(mediaId) : await unlikeMedia(mediaId)
      setLiked(resp.likedByDevice)
      setLikeCount(resp.likeCount)
    } catch {
      setLiked(liked)
      setLikeCount(likeCount)
    } finally {
      setBusy(false)
    }
  }

  return (
    <button
      type="button"
      className={`like-button${liked ? ' liked' : ''}`}
      onClick={toggle}
      disabled={busy}
      aria-pressed={liked}
      aria-label={liked ? 'Unlike' : 'Like'}
    >
      <span aria-hidden="true">{liked ? '\u2764\ufe0f' : '\u{1f90d}'}</span>
      <span className="like-count">{likeCount}</span>
    </button>
  )
}
