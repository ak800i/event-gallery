import { beforeEach, describe, expect, it } from 'vitest'
import { getDeviceId } from './useDeviceId'

describe('getDeviceId', () => {
  beforeEach(() => {
    localStorage.clear()
  })

  it('generates and persists a device id', () => {
    const id1 = getDeviceId()
    expect(id1).toBeTruthy()
    const id2 = getDeviceId()
    expect(id2).toBe(id1)
  })

  it('persists the id across calls via localStorage', () => {
    const id = getDeviceId()
    expect(localStorage.getItem('wg_device_id')).toBe(id)
  })
})
