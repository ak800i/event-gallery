import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import * as apiClient from '../api/client'
import { DEFAULT_BRANDING } from '../utils/branding'
import { AdminBrandingEditor } from './AdminBrandingEditor'

vi.mock('../api/client', () => ({
  adminGetBranding: vi.fn(),
  adminUpdateBranding: vi.fn(),
  adminResetBranding: vi.fn(),
}))

describe('AdminBrandingEditor', () => {
  beforeEach(() => {
    vi.mocked(apiClient.adminGetBranding).mockReset().mockResolvedValue(DEFAULT_BRANDING)
    vi.mocked(apiClient.adminUpdateBranding).mockReset().mockImplementation(async (value) => value)
    vi.mocked(apiClient.adminResetBranding).mockReset().mockResolvedValue(DEFAULT_BRANDING)
  })

  it('previews arbitrary plain text and full-spectrum color choices before saving', async () => {
    render(<AdminBrandingEditor />)
    const user = userEvent.setup()

    const title = await screen.findByLabelText('Page title')
    fireEvent.change(title, { target: { value: '<b>Sam & Alex</b>' } })
    fireEvent.change(screen.getByLabelText('Primary accent color picker'), { target: { value: '#12abef' } })

    expect(screen.getByLabelText('Live main-page preview')).toHaveTextContent('<b>Sam & Alex</b>')
    await user.click(screen.getByRole('button', { name: /save customization/i }))

    await waitFor(() =>
      expect(apiClient.adminUpdateBranding).toHaveBeenCalledWith(
        expect.objectContaining({ pageTitle: '<b>Sam & Alex</b>', primaryColor: '#12abef' }),
      ),
    )
  })

  it('resets all values after explicit confirmation', async () => {
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(true)
    render(<AdminBrandingEditor />)
    const user = userEvent.setup()

    await user.click(await screen.findByRole('button', { name: /reset defaults/i }))
    await waitFor(() => expect(apiClient.adminResetBranding).toHaveBeenCalledOnce())
    expect(screen.getByText(/reset to defaults/i)).toBeInTheDocument()
    confirm.mockRestore()
  })
})
