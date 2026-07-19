import { render, screen, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { Gallery } from './Gallery'
import * as apiClient from '../api/client'
import type { MediaItem } from '../types'

vi.mock('../api/client', async () => {
  const actual = await vi.importActual<typeof apiClient>('../api/client')
  return {
    ...actual,
    fetchGallery: vi.fn(),
    likeMedia: vi.fn(),
    unlikeMedia: vi.fn(),
  }
})

function makeItem(overrides: Partial<MediaItem> = {}): MediaItem {
  return {
    id: overrides.id ?? 'id1',
    originalFilename: 'photo.jpg',
    kind: 'image',
    mimeType: 'image/jpeg',
    sizeBytes: 1000,
    hasThumbnail: true,
    uploadedAt: '2024-01-01T00:00:00Z',
    uploaderName: 'Alex',
    likeCount: 0,
    likedByDevice: false,
    ...overrides,
  }
}

describe('Gallery', () => {
  beforeEach(() => {
    vi.mocked(apiClient.fetchGallery).mockReset()
  })

  it('shows an empty state when there are no items', async () => {
    vi.mocked(apiClient.fetchGallery).mockResolvedValue({ items: [], nextCursor: '' })
    render(<Gallery />)
    await waitFor(() => expect(screen.getByText(/be the first to upload/i)).toBeInTheDocument())
  })

  it('renders returned items', async () => {
    vi.mocked(apiClient.fetchGallery).mockResolvedValue({
      items: [makeItem({ id: 'id1' }), makeItem({ id: 'id2', originalFilename: 'clip.mp4', kind: 'video' })],
      nextCursor: '',
    })
    render(<Gallery />)
    await waitFor(() => expect(screen.getAllByRole('button', { name: /open/i })).toHaveLength(2))
  })

  it('shows an error message when the fetch fails', async () => {
    vi.mocked(apiClient.fetchGallery).mockRejectedValue(new Error('network down'))
    render(<Gallery />)
    await waitFor(() => expect(screen.getByText(/failed to load the gallery/i)).toBeInTheDocument())
  })

  it('reloads with new sort order when the sort control changes', async () => {
    vi.mocked(apiClient.fetchGallery).mockResolvedValue({ items: [makeItem()], nextCursor: '' })
    render(<Gallery />)
    await waitFor(() => expect(apiClient.fetchGallery).toHaveBeenCalled())

    const select = screen.getByLabelText(/sort by/i)
    const { default: userEvent } = await import('@testing-library/user-event')
    await userEvent.setup().selectOptions(select, 'captured')

    await waitFor(() =>
      expect(apiClient.fetchGallery).toHaveBeenCalledWith(expect.objectContaining({ sort: 'captured' })),
    )
  })

  it('opens the lightbox when a media card is clicked', async () => {
    vi.mocked(apiClient.fetchGallery).mockResolvedValue({ items: [makeItem()], nextCursor: '' })
    render(<Gallery />)
    const openButton = await screen.findByRole('button', { name: /open/i })

    const { default: userEvent } = await import('@testing-library/user-event')
    await userEvent.setup().click(openButton)

    expect(screen.getByRole('dialog')).toBeInTheDocument()
  })
})
