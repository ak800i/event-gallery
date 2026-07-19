import { createSHA256 } from 'hash-wasm'

const CHUNK_SIZE = 8 * 1024 * 1024 // 8 MiB

/**
 * Computes the SHA-256 digest of an entire File/Blob, streaming it in
 * fixed-size chunks so we never hold the whole file in memory at once
 * (important for phone videos that can be hundreds of MB). Used for the
 * client-side duplicate-upload preflight check; the server independently
 * re-computes this hash after the upload completes and is the source of
 * truth.
 */
export async function sha256OfFile(file: Blob, onProgress?: (fraction: number) => void): Promise<string> {
  const hasher = await createSHA256()
  hasher.init()

  let offset = 0
  while (offset < file.size) {
    const end = Math.min(offset + CHUNK_SIZE, file.size)
    const chunk = file.slice(offset, end)
    const buffer = new Uint8Array(await chunk.arrayBuffer())
    hasher.update(buffer)
    offset = end
    onProgress?.(offset / file.size)
  }
  if (file.size === 0) {
    onProgress?.(1)
  }

  return hasher.digest('hex')
}
