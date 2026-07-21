import { describe, expect, it } from 'vitest'
import { sha256OfFile } from '../utils/hash'

// Known SHA-256 digest of the UTF-8 bytes "hello event gallery", verified
// independently via Python's hashlib module.
const KNOWN_TEXT = 'hello event gallery'
const KNOWN_SHA256 = '17a5acba103302bdaee171596e800da8e88dc1dfe7f4ea78abbf0e1d75a4cec7'

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
