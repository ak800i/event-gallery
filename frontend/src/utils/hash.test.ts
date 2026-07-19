import { describe, expect, it } from 'vitest'
import { sha256OfFile } from '../utils/hash'

// Known SHA-256 digest of the UTF-8 bytes "hello wedding gallery", verified
// independently via Node's crypto module.
const KNOWN_TEXT = 'hello wedding gallery'
const KNOWN_SHA256 = 'a9c57ed3ede712770481b13667fd0b69be7d2eb6f09cffdfa3fc1889f363fca3'

describe('sha256OfFile', () => {
  it('matches a known reference digest', async () => {
    const digest = await sha256OfFile(new Blob([KNOWN_TEXT]))
    expect(digest).toBe(KNOWN_SHA256)
  })

  it('is deterministic for the same content', async () => {
    const blob = new Blob([KNOWN_TEXT])
    const digest1 = await sha256OfFile(blob)
    const digest2 = await sha256OfFile(blob)
    expect(digest1).toBe(digest2)
  })

  it('produces different digests for different content', async () => {
    const a = await sha256OfFile(new Blob(['content-a']))
    const b = await sha256OfFile(new Blob(['content-b']))
    expect(a).not.toBe(b)
  })

  it('reports progress up to 1 for an empty file', async () => {
    const progressValues: number[] = []
    await sha256OfFile(new Blob([]), (p) => progressValues.push(p))
    expect(progressValues.at(-1)).toBe(1)
  })
})
