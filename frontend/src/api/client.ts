import type {
  AuditLogResponse,
  BrandingConfig,
  GalleryResponse,
  GallerySort,
  LikeResponse,
  MediaStatus,
  PublicConfig,
  SortOrder,
  UploadCheckResponse,
} from '../types'
import { getDeviceId } from '../hooks/useDeviceId'

export class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    ...init,
    headers: {
      ...(init?.body ? { 'Content-Type': 'application/json' } : {}),
      'X-Device-Id': getDeviceId(),
      ...(init?.headers ?? {}),
    },
    credentials: 'same-origin',
  })
  if (!res.ok) {
    let message = `Request failed with status ${res.status}`
    try {
      const body = await res.json()
      if (body?.error) message = body.error
    } catch {
      // ignore body parse errors, fall back to generic message
    }
    throw new ApiError(res.status, message)
  }
  if (res.status === 204) return undefined as T
  return (await res.json()) as T
}

export function fetchGallery(params: {
  cursor?: string
  sort?: GallerySort
  order?: SortOrder
  limit?: number
}): Promise<GalleryResponse> {
  const q = new URLSearchParams()
  if (params.cursor) q.set('cursor', params.cursor)
  if (params.sort) q.set('sort', params.sort)
  if (params.order) q.set('order', params.order)
  if (params.limit) q.set('limit', String(params.limit))
  return request(`/api/gallery?${q.toString()}`)
}

export function fetchPublicConfig(): Promise<PublicConfig> {
  return request('/api/config/public')
}

export function checkUploadDuplicate(sha256: string, size: number, filename: string): Promise<UploadCheckResponse> {
  return request('/api/uploads/check', {
    method: 'POST',
    body: JSON.stringify({ sha256, size, filename }),
  })
}

export function likeMedia(id: string): Promise<LikeResponse> {
  return request(`/api/media/${encodeURIComponent(id)}/like`, { method: 'POST' })
}

export function unlikeMedia(id: string): Promise<LikeResponse> {
  return request(`/api/media/${encodeURIComponent(id)}/like`, { method: 'DELETE' })
}

export function mediaThumbnailUrl(id: string): string {
  return `/api/media/${encodeURIComponent(id)}/thumbnail`
}

export function mediaFileUrl(id: string): string {
  return `/api/media/${encodeURIComponent(id)}/file`
}

export function mediaDownloadUrl(id: string): string {
  return `/api/media/${encodeURIComponent(id)}/download`
}

// --- Admin API ---

let cachedCsrfToken: string | null = null

function csrfHeaders(): Record<string, string> {
  return cachedCsrfToken ? { 'X-CSRF-Token': cachedCsrfToken } : {}
}

export async function adminLogin(password: string): Promise<void> {
  const resp = await request<{ csrfToken: string }>('/api/admin/login', {
    method: 'POST',
    body: JSON.stringify({ password }),
  })
  cachedCsrfToken = resp.csrfToken
}

export async function adminCheckSession(): Promise<boolean> {
  try {
    const resp = await request<{ authenticated: boolean; csrfToken: string }>('/api/admin/session')
    cachedCsrfToken = resp.csrfToken
    return resp.authenticated
  } catch {
    return false
  }
}

export async function adminLogout(): Promise<void> {
  await request('/api/admin/logout', { method: 'POST', headers: csrfHeaders() })
  cachedCsrfToken = null
}

export function adminListMedia(params: { status?: MediaStatus | ''; cursor?: string; limit?: number }): Promise<GalleryResponse> {
  const q = new URLSearchParams()
  if (params.status) q.set('status', params.status)
  if (params.cursor) q.set('cursor', params.cursor)
  if (params.limit) q.set('limit', String(params.limit))
  return request(`/api/admin/media?${q.toString()}`)
}

export function adminBulkDelete(ids: string[]): Promise<{ changed: string[] }> {
  return request('/api/admin/media/bulk-delete', {
    method: 'POST',
    body: JSON.stringify({ ids }),
    headers: csrfHeaders(),
  })
}

export function adminBulkRestore(ids: string[]): Promise<{ changed: string[] }> {
  return request('/api/admin/media/bulk-restore', {
    method: 'POST',
    body: JSON.stringify({ ids }),
    headers: csrfHeaders(),
  })
}

export function adminAuditLog(cursor?: string): Promise<AuditLogResponse> {
  const q = new URLSearchParams()
  if (cursor) q.set('cursor', cursor)
  return request(`/api/admin/audit-log?${q.toString()}`)
}

export function adminGetConfig(): Promise<{ uploadExpiresAt?: string }> {
  return request('/api/admin/config')
}

export function adminUpdateConfig(uploadExpiresAt: string | null): Promise<{ uploadExpiresAt?: string }> {
  return request('/api/admin/config', {
    method: 'PUT',
    body: JSON.stringify({ uploadExpiresAt: uploadExpiresAt ?? '' }),
    headers: csrfHeaders(),
  })
}

export function adminGetBranding(): Promise<BrandingConfig> {
  return request('/api/admin/branding')
}

export function adminUpdateBranding(branding: BrandingConfig): Promise<BrandingConfig> {
  return request('/api/admin/branding', {
    method: 'PUT',
    body: JSON.stringify(branding),
    headers: csrfHeaders(),
  })
}

export function adminResetBranding(): Promise<BrandingConfig> {
  return request('/api/admin/branding', {
    method: 'DELETE',
    headers: csrfHeaders(),
  })
}
