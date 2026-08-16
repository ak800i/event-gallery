import { act, fireEvent, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import type { PublicConfig } from '../types'
import { DEFAULT_BRANDING } from '../utils/branding'
import { UploadPanel } from './UploadPanel'

const config: PublicConfig = {
  uploadsEnabled: true,
  approvalRequired: false,
  maxUploadBytes: 5 * 1024 * 1024 * 1024,
  uploadConcurrency: 50,
  allowedImageMimeTypes: ['image/jpeg', 'image/png'],
  allowedVideoMimeTypes: ['video/mp4'],
  guestNameMaxLength: 60,
  branding: DEFAULT_BRANDING,
}

describe('UploadPanel', () => {
  it('keeps the uploader compact until requested and opens a webcam-free Uppy modal', async () => {
    render(<UploadPanel guestName="Alex" config={config} branding={DEFAULT_BRANDING} onUploadComplete={vi.fn()} />)

    const trigger = screen.getByRole('button', { name: /add photos & videos/i })
    expect(trigger).toHaveTextContent(/uploads resume automatically/i)
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()

    await userEvent.setup().click(trigger)

    const dialog = await screen.findByRole('dialog')
    expect(dialog).toBeInTheDocument()
    expect(screen.getByText(/drop files here/i)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /camera|webcam/i })).not.toBeInTheDocument()

    await userEvent.setup().click(screen.getByRole('button', { name: /close modal/i }))
    expect(dialog.closest('.uppy-Dashboard')).toHaveClass('uppy-Dashboard--isClosing')
    // Uppy debounces focus restoration; let it settle before jsdom teardown.
    await act(() => new Promise((resolve) => setTimeout(resolve, 600)))
  })

  it('offers the native picker a wildcard accept but still filters by the configured MIME types', async () => {
    render(<UploadPanel guestName="Alex" config={config} branding={DEFAULT_BRANDING} onUploadComplete={vi.fn()} />)
    await userEvent.setup().click(screen.getByRole('button', { name: /add photos & videos/i }))

    const input = document.querySelector<HTMLInputElement>('.uppy-Dashboard-input:not([webkitdirectory])')!
    // Anything narrower makes iOS transcode every picked asset before the picker closes.
    expect(input).toHaveAttribute('accept', 'image/*, video/*')

    fireEvent.change(input, {
      target: {
        files: [
          new File(['a'], 'photo.jpg', { type: 'image/jpeg' }),
          new File(['b'], 'notes.pdf', { type: 'application/pdf' }),
        ],
      },
    })

    expect(await screen.findByText('photo.jpg')).toBeInTheDocument()
    expect(screen.queryByText('notes.pdf')).not.toBeInTheDocument()
    const alerts = await screen.findAllByRole('alert')
    expect(alerts.some((alert) => /"notes\.pdf" is not a supported file type/i.test(alert.textContent ?? ''))).toBe(true)
  })

  it('shows a compact closed state when uploads are disabled', () => {
    render(<UploadPanel
        guestName="Alex"
        config={{ ...config, uploadsEnabled: false }}
        branding={DEFAULT_BRANDING}
        onUploadComplete={vi.fn()}
      />)
    expect(screen.getByText(/uploads are closed/i)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /add photos/i })).not.toBeInTheDocument()
  })
})
