import type { HttpRequest } from 'tus-js-client'
import type { UppyFile } from '@uppy/core'

const CHUNK_SIZE = 8 * 1024 * 1024 // 8 MiB; must match the `chunkSize` tus option.

/**
 * Attaches an `Upload-Checksum: sha256 <base64>` header to a tus PATCH
 * request, computed from the exact byte range this request is about to
 * send. tusd (configured with the checksum extension) verifies this on
 * receipt and rejects the chunk with a 460 response if it doesn't match,
 * which tus-js-client/@uppy/tus then retries automatically.
 *
 * tus-js-client's `onBeforeRequest` only gives us the outgoing HttpRequest,
 * not the bytes being sent -- but @uppy/tus additionally passes the source
 * UppyFile, so we can independently re-slice the same underlying Blob using
 * the `Upload-Offset` header (which tus-js-client has already set on `req`
 * by the time this runs) and our fixed chunk size.
 */
export async function attachChunkChecksum(
  req: HttpRequest,
  file: UppyFile<Record<string, unknown>, Record<string, unknown>>,
): Promise<void> {
  if (req.getMethod() !== 'PATCH') return
  const data = file.data as Blob
  if (!data || typeof data.slice !== 'function') return

  const offsetHeader = req.getHeader('Upload-Offset')
  const offset = offsetHeader ? parseInt(offsetHeader, 10) : 0
  if (Number.isNaN(offset)) return

  const end = Math.min(offset + CHUNK_SIZE, data.size)
  const checksum = await sha256ChunkBase64(data, offset, end)
  req.setHeader('Upload-Checksum', `sha256 ${checksum}`)
}

async function sha256ChunkBase64(file: Blob, start: number, end: number): Promise<string> {
  const slice = file.slice(start, end)
  const buffer = await slice.arrayBuffer()
  const digest = await crypto.subtle.digest('SHA-256', buffer)
  return arrayBufferToBase64(digest)
}

function arrayBufferToBase64(buffer: ArrayBuffer): string {
  const bytes = new Uint8Array(buffer)
  let binary = ''
  for (let i = 0; i < bytes.length; i++) {
    binary += String.fromCharCode(bytes[i])
  }
  return btoa(binary)
}

export const TUS_CHUNK_SIZE = CHUNK_SIZE
