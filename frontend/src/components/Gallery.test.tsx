import { act, render, screen, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { Gallery } from './Gallery'
import * as apiClient from '../api/client'
import type { BrandingConfig, MediaItem } from '../types'
import { DEFAULT_BRANDING } from '../utils/branding'

vi.mock('../api/client', async () => {
  const actual = await vi.importActual<typeof apiClient>('../api/client')
  return {
    ...actual,
    fetchGallery: vi.fn(),
    likeMedia: vi.fn(),
    unlikeMedia: vi.fn(),
  }
})

// Gallery owns open/index state; YARL's rendering and slide mapping are
// covered separately in Lightbox.test.tsx. Keeping this boundary mocked makes
// these tests deterministic instead of waiting on third-party animations.
vi.mock('./Lightbox', () => ({
  Lightbox: ({
    items,
    index,
    onClose,
    onIndexChange,
  }: {
    items: MediaItem[]
    index: number
    onClose: () => void
    onIndexChange: (index: number) => void
  }) => (
    <div role="dialog" aria-label="Lightbox">
      <button type="button" aria-label="Previous" disabled={index === 0} onClick={() => onIndexChange(index - 1)} />
      <button
        type="button"
        aria-label="Next"
        disabled={index === items.length - 1}
        onClick={() => onIndexChange(index + 1)}
      />
      <button type="button" aria-label="Close" onClick={onClose} />
    </div>
  ),
}))

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

function renderGallery(branding: BrandingConfig = DEFAULT_BRANDING) {
  return render(<Gallery branding={branding} />)
}

describe('Gallery', () => {
  beforeEach(() => {
    vi.mocked(apiClient.fetchGallery).mockReset()
  })

  it('shows the configured empty state when there are no items', async () => {
    vi.mocked(apiClient.fetchGallery).mockResolvedValue({ items: [], nextCursor: '' })
    renderGallery({ ...DEFAULT_BRANDING, emptyGalleryText: 'Bring on the memories!' })
    await waitFor(() => expect(screen.getByText('Bring on the memories!')).toBeInTheDocument())
  })

  it('renders returned items', async () => {
    vi.mocked(apiClient.fetchGallery).mockResolvedValue({
      items: [makeItem({ id: 'id1' }), makeItem({ id: 'id2', originalFilename: 'clip.mp4', kind: 'video' })],
      nextCursor: '',
    })
    renderGallery()
    await waitFor(() => expect(screen.getAllByRole('button', { name: /open/i })).toHaveLength(2))
    const download = screen.getAllByRole('link', { name: /download original/i })[0]
    expect(download.querySelector('svg')).toBeInTheDocument()
    expect(download).not.toHaveTextContent('⬇️')
  })

  it('shows the configured error message when the fetch fails', async () => {
    vi.mocked(apiClient.fetchGallery).mockRejectedValue(new Error('network down'))
    renderGallery({ ...DEFAULT_BRANDING, galleryErrorText: 'Please try again later.' })
    await waitFor(() => expect(screen.getByText('Please try again later.')).toBeInTheDocument())
  })

  it('reloads with new sort order when the sort control changes', async () => {
    vi.mocked(apiClient.fetchGallery).mockResolvedValue({ items: [makeItem()], nextCursor: '' })
    renderGallery()
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
    renderGallery()
    const openButton = await screen.findByRole('button', { name: /open/i })

    const { default: userEvent } = await import('@testing-library/user-event')
    await userEvent.setup().click(openButton)

    expect(await screen.findByRole('dialog')).toBeInTheDocument()
  })

  it('provides accessible previous and next controls for lightbox navigation', async () => {
    vi.mocked(apiClient.fetchGallery).mockResolvedValue({
      items: [makeItem({ id: 'id1' }), makeItem({ id: 'id2', originalFilename: 'second.jpg' })],
      nextCursor: '',
    })
    renderGallery()

    const { default: userEvent } = await import('@testing-library/user-event')
    const user = userEvent.setup()
    const openButton = (await screen.findAllByRole('button', { name: /open/i }))[0]
    await act(async () => {
      await user.click(openButton)
    })

    await screen.findByRole('dialog')
    const previous = screen.getByRole('button', { name: /previous/i })
    const next = screen.getByRole('button', { name: /next/i })
    expect(previous).toBeDisabled()
    expect(next).toBeEnabled()

    await act(async () => {
      await user.click(screen.getByRole('button', { name: /close/i }))
    })
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
  })
})
