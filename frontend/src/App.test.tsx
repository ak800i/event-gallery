import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { fetchPublicConfig } from './api/client'
import { DEFAULT_BRANDING } from './utils/branding'
import type { PublicConfig } from './types'
import { App } from './App'

vi.mock('./api/client', () => ({ fetchPublicConfig: vi.fn() }))
vi.mock('./components/AdminApp', () => ({ AdminApp: () => <div>Admin</div> }))
vi.mock('./components/GuestNameEditor', () => ({ GuestNameEditor: () => null }))
vi.mock('./components/UploadPanel', () => ({
  UploadPanel: ({ onUploadComplete }: { onUploadComplete: (count: number) => Promise<boolean> }) => (
    <button type="button" onClick={() => void onUploadComplete(1)}>
      Simulate upload completion
    </button>
  ),
}))
vi.mock('./components/Gallery', () => ({
  Gallery: ({ refreshRequest }: { refreshRequest: { id: number; expectedUploads: number } }) => (
    <output aria-label="refresh request">{JSON.stringify(refreshRequest)}</output>
  ),
}))

function config(approvalRequired: boolean): PublicConfig {
  return {
    uploadsEnabled: true,
    approvalRequired,
    maxUploadBytes: 1024,
    uploadConcurrency: 3,
    allowedImageMimeTypes: ['image/jpeg'],
    allowedVideoMimeTypes: ['video/mp4'],
    guestNameMaxLength: 60,
    branding: DEFAULT_BRANDING,
  }
}

describe('post-upload approval behavior', () => {
  beforeEach(() => {
    window.history.replaceState({}, '', '/')
    vi.mocked(fetchPublicConfig).mockReset()
  })

  it('does not trigger gallery polling when approval is required', async () => {
    vi.mocked(fetchPublicConfig).mockResolvedValue(config(true))
    render(<App />)
    await userEvent.setup().click(await screen.findByRole('button', { name: /simulate upload/i }))
    await waitFor(() => expect(fetchPublicConfig).toHaveBeenCalledTimes(2))
    expect(screen.getByLabelText('refresh request')).toHaveTextContent('{"id":0,"expectedUploads":0}')
  })

  it('retains automatic gallery refresh when approval is off', async () => {
    vi.mocked(fetchPublicConfig).mockResolvedValue(config(false))
    render(<App />)
    await userEvent.setup().click(await screen.findByRole('button', { name: /simulate upload/i }))
    await waitFor(() => expect(screen.getByLabelText('refresh request')).toHaveTextContent('{"id":1,"expectedUploads":1}'))
  })
})
