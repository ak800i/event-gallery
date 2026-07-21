import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import * as apiClient from '../api/client'
import type { MediaItem } from '../types'
import { AdminMediaList, AdminModerationSettings } from './AdminDashboard'

vi.mock('../api/client', async () => {
  const actual = await vi.importActual<typeof apiClient>('../api/client')
  return {
    ...actual,
    adminListMedia: vi.fn(),
    adminBulkApprove: vi.fn(),
    adminBulkDelete: vi.fn(),
    adminBulkRestore: vi.fn(),
    adminGetModeration: vi.fn(),
    adminUpdateModeration: vi.fn(),
    adminMediaThumbnailUrl: (id: string) => `/api/admin/media/${id}/thumbnail`,
  }
})

const pending: MediaItem = {
  id: 'pending-id',
  originalFilename: 'pending.jpg',
  kind: 'image',
  mimeType: 'image/jpeg',
  sizeBytes: 100,
  hasThumbnail: true,
  uploadedAt: '2026-01-01T00:00:00Z',
  uploaderName: 'Guest',
  likeCount: 0,
  likedByDevice: false,
  status: 'active',
}

describe('Admin approval queue', () => {
  beforeEach(() => {
    vi.mocked(apiClient.adminListMedia).mockReset().mockResolvedValue({ items: [pending], nextCursor: '' })
    vi.mocked(apiClient.adminBulkApprove).mockReset().mockResolvedValue({ changed: ['pending-id'] })
    vi.mocked(apiClient.adminGetModeration).mockReset()
    vi.mocked(apiClient.adminUpdateModeration).mockReset()
  })

  it('lists pending media with protected thumbnails and bulk approval', async () => {
    render(<AdminMediaList status="pending" />)
    expect(await screen.findByText('pending.jpg')).toBeInTheDocument()
    expect(apiClient.adminListMedia).toHaveBeenCalledWith({ status: 'pending', cursor: undefined, limit: 60 })
    expect(document.querySelector('img')).toHaveAttribute('src', '/api/admin/media/pending-id/thumbnail')

    const user = userEvent.setup()
    await user.click(screen.getByRole('checkbox'))
    await user.click(screen.getByRole('button', { name: /approve selected/i }))
    expect(apiClient.adminBulkApprove).toHaveBeenCalledWith(['pending-id'])
    await waitFor(() => expect(screen.queryByText('pending.jpg')).not.toBeInTheDocument())
  })

  it('confirms disabling and reports automatic approvals', async () => {
    vi.mocked(apiClient.adminGetModeration).mockResolvedValue({ approvalRequired: true, autoApproved: 0 })
    vi.mocked(apiClient.adminUpdateModeration).mockResolvedValue({ approvalRequired: false, autoApproved: 7 })
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(true)
    render(<AdminModerationSettings />)

    const user = userEvent.setup()
    const toggle = await screen.findByRole('checkbox', { name: /require admin approval/i })
    await user.click(toggle)
    await user.click(screen.getByRole('button', { name: /save approval setting/i }))

    expect(confirm).toHaveBeenCalled()
    expect(apiClient.adminUpdateModeration).toHaveBeenCalledWith(false)
    expect(await screen.findByText(/7 pending item\(s\) automatically approved/i)).toBeInTheDocument()
    confirm.mockRestore()
  })
})
