import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { LikeButton } from './LikeButton'
import * as apiClient from '../api/client'

vi.mock('../api/client', async () => {
  const actual = await vi.importActual<typeof apiClient>('../api/client')
  return {
    ...actual,
    likeMedia: vi.fn(),
    unlikeMedia: vi.fn(),
  }
})

describe('LikeButton', () => {
  beforeEach(() => {
    vi.mocked(apiClient.likeMedia).mockReset()
    vi.mocked(apiClient.unlikeMedia).mockReset()
  })

  it('renders the initial like count and state', () => {
    render(<LikeButton mediaId="id1" initialLikeCount={3} initialLiked={false} />)
    expect(screen.getByText('3')).toBeInTheDocument()
    expect(screen.getByRole('button')).toHaveAttribute('aria-pressed', 'false')
  })

  it('likes on click and reflects the server response', async () => {
    vi.mocked(apiClient.likeMedia).mockResolvedValue({ likeCount: 4, likedByDevice: true })
    const user = userEvent.setup()
    render(<LikeButton mediaId="id1" initialLikeCount={3} initialLiked={false} />)

    await user.click(screen.getByRole('button'))

    await waitFor(() => expect(screen.getByText('4')).toBeInTheDocument())
    expect(screen.getByRole('button')).toHaveAttribute('aria-pressed', 'true')
    expect(apiClient.likeMedia).toHaveBeenCalledWith('id1')
  })

  it('unlikes when already liked', async () => {
    vi.mocked(apiClient.unlikeMedia).mockResolvedValue({ likeCount: 2, likedByDevice: false })
    const user = userEvent.setup()
    render(<LikeButton mediaId="id1" initialLikeCount={3} initialLiked={true} />)

    await user.click(screen.getByRole('button'))

    await waitFor(() => expect(screen.getByText('2')).toBeInTheDocument())
    expect(apiClient.unlikeMedia).toHaveBeenCalledWith('id1')
  })

  it('rolls back the optimistic update if the request fails', async () => {
    vi.mocked(apiClient.likeMedia).mockRejectedValue(new Error('network error'))
    const user = userEvent.setup()
    render(<LikeButton mediaId="id1" initialLikeCount={3} initialLiked={false} />)

    await user.click(screen.getByRole('button'))

    await waitFor(() => expect(screen.getByText('3')).toBeInTheDocument())
    expect(screen.getByRole('button')).toHaveAttribute('aria-pressed', 'false')
  })
})
