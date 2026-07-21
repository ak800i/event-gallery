import { act, renderHook } from '@testing-library/react'
import { beforeEach, describe, expect, it } from 'vitest'
import { readStoredGuestName, useGuestName } from './useGuestName'

describe('useGuestName', () => {
  beforeEach(() => {
    localStorage.clear()
  })

  it('starts empty when nothing is stored', () => {
    const { result } = renderHook(() => useGuestName())
    expect(result.current[0]).toBe('')
  })

  it('persists the name to localStorage and reflects it in state', () => {
    const { result } = renderHook(() => useGuestName())
    act(() => {
      result.current[1]('Alex')
    })
    expect(result.current[0]).toBe('Alex')
    expect(localStorage.getItem('eg_guest_name')).toBe('Alex')
  })

  it('readStoredGuestName reads the persisted value outside React', () => {
    const { result } = renderHook(() => useGuestName())
    act(() => {
      result.current[1]('Jordan')
    })
    expect(readStoredGuestName()).toBe('Jordan')
  })
})
