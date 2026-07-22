import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  ApiError,
  adminBulkApprove,
  adminBulkDelete,
  adminBulkPurge,
  adminLogin,
  adminUpdateBranding,
  adminUpdateModeration,
  checkUploadDuplicate,
  fetchGallery,
} from './client'
import { DEFAULT_BRANDING } from '../utils/branding'

function jsonResponse(body: unknown, init?: ResponseInit): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
    ...init,
  })
}

describe('api client', () => {
  beforeEach(() => {
    localStorage.clear()
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('fetchGallery issues a GET with query params and parses the response', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ items: [], nextCursor: '' }))
    vi.stubGlobal('fetch', fetchMock)

    const resp = await fetchGallery({ sort: 'captured', order: 'asc', limit: 10 })
    expect(resp.items).toEqual([])

    const [url, init] = fetchMock.mock.calls[0]
    expect(String(url)).toContain('/api/gallery?')
    expect(String(url)).toContain('sort=captured')
    expect(String(url)).toContain('order=asc')
    expect(String(url)).toContain('limit=10')
    expect((init.headers as Record<string, string>)['X-Device-Id']).toBeTruthy()
  })

  it('throws ApiError with the server-provided message on failure', async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ error: 'boom' }), { status: 400 }),
    )
    vi.stubGlobal('fetch', fetchMock)

    await expect(checkUploadDuplicate('a'.repeat(64), 100, 'x.jpg')).rejects.toMatchObject(
      new ApiError(400, 'boom'),
    )
  })

  it('falls back to a generic message when the error body is not JSON', async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response('not json', { status: 500 }))
    vi.stubGlobal('fetch', fetchMock)

    await expect(fetchGallery({})).rejects.toThrow('Request failed with status 500')
  })

  it('adminLogin caches the csrf token for subsequent mutating admin calls', async () => {
    const loginResponse = jsonResponse({ csrfToken: 'csrf-abc' })
    const deleteFetch = jsonResponse({ changed: ['id1'] })
    const brandingFetch = jsonResponse(DEFAULT_BRANDING)
    const approvalFetch = jsonResponse({ changed: ['id2'] })
    const purgeFetch = jsonResponse({ changed: ['id3'] })
    const moderationFetch = jsonResponse({ approvalRequired: true, autoApproved: 0 })
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(loginResponse)
      .mockResolvedValueOnce(deleteFetch)
      .mockResolvedValueOnce(brandingFetch)
      .mockResolvedValueOnce(approvalFetch)
      .mockResolvedValueOnce(purgeFetch)
      .mockResolvedValueOnce(moderationFetch)
    vi.stubGlobal('fetch', fetchMock)

    await adminLogin('password123')
    await adminBulkDelete(['id1'])
    await adminUpdateBranding(DEFAULT_BRANDING)
    await adminBulkApprove(['id2'])
    await adminBulkPurge(['id3'])
    await adminUpdateModeration(true)

    const [, deleteInit] = fetchMock.mock.calls[1]
    expect((deleteInit.headers as Record<string, string>)['X-CSRF-Token']).toBe('csrf-abc')
    const [brandingURL, brandingInit] = fetchMock.mock.calls[2]
    expect(brandingURL).toBe('/api/admin/branding')
    expect(brandingInit.method).toBe('PUT')
    expect((brandingInit.headers as Record<string, string>)['X-CSRF-Token']).toBe('csrf-abc')
    expect(fetchMock.mock.calls[3][0]).toBe('/api/admin/media/bulk-approve')
    expect((fetchMock.mock.calls[3][1].headers as Record<string, string>)['X-CSRF-Token']).toBe('csrf-abc')
    expect(fetchMock.mock.calls[4][0]).toBe('/api/admin/media/bulk-purge')
    expect((fetchMock.mock.calls[4][1].headers as Record<string, string>)['X-CSRF-Token']).toBe('csrf-abc')
    expect(fetchMock.mock.calls[5][0]).toBe('/api/admin/moderation')
    expect((fetchMock.mock.calls[5][1].headers as Record<string, string>)['X-CSRF-Token']).toBe('csrf-abc')
  })
})
