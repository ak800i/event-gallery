import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { GuestNameEditor } from './GuestNameEditor'

describe('GuestNameEditor', () => {
  it('shows the edit form immediately when no name is set', () => {
    render(<GuestNameEditor guestName="" onSave={vi.fn()} maxLength={60} />)
    expect(screen.getByLabelText(/your name/i)).toBeInTheDocument()
  })

  it('shows a display view with a change link when a name is already set', () => {
    render(<GuestNameEditor guestName="Alex" onSave={vi.fn()} maxLength={60} />)
    expect(screen.getByText(/posting as/i)).toBeInTheDocument()
    expect(screen.getByText('Alex')).toBeInTheDocument()
  })

  it('calls onSave with the trimmed name on submit', async () => {
    const onSave = vi.fn()
    const user = userEvent.setup()
    render(<GuestNameEditor guestName="" onSave={onSave} maxLength={60} />)

    await user.type(screen.getByLabelText(/your name/i), '  Jamie  ')
    await user.click(screen.getByRole('button', { name: /save/i }))

    expect(onSave).toHaveBeenCalledWith('Jamie')
  })

  it('switches back to display mode after saving', async () => {
    const user = userEvent.setup()
    render(<GuestNameEditor guestName="" onSave={vi.fn()} maxLength={60} />)
    await user.type(screen.getByLabelText(/your name/i), 'Sam')
    await user.click(screen.getByRole('button', { name: /save/i }))
    expect(screen.getByText(/posting as/i)).toBeInTheDocument()
  })

  it('allows re-entering edit mode via the change button', async () => {
    const user = userEvent.setup()
    render(<GuestNameEditor guestName="Alex" onSave={vi.fn()} maxLength={60} />)
    await user.click(screen.getByRole('button', { name: /change/i }))
    expect(screen.getByLabelText(/your name/i)).toBeInTheDocument()
  })
})
