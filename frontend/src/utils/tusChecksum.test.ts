import { describe, expect, it, vi } from 'vitest'
import { attachChunkChecksum, TUS_CHUNK_SIZE } from '../utils/tusChecksum'
import type { HttpRequest } from 'tus-js-client'
import type { UppyFile } from '@uppy/core'

function fakeRequest(method: string, headers: Record<string, string>): HttpRequest {
  return {
    getMethod: () => method,
    getURL: () => 'http://example.test/files/abc',
    setHeader: vi.fn(),
    getHeader: (name: string) => headers[name],
    setProgressHandler: vi.fn(),
    send: vi.fn(),
    abort: vi.fn(),
    getUnderlyingObject: vi.fn(),
  } as unknown as HttpRequest
}

function fakeFile(data: Blob): UppyFile<Record<string, unknown>, Record<string, unknown>> {
  return { data } as unknown as UppyFile<Record<string, unknown>, Record<string, unknown>>
}

describe('attachChunkChecksum', () => {
  it('sets an Upload-Checksum header for PATCH requests', async () => {
    const req = fakeRequest('PATCH', { 'Upload-Offset': '0' })
    const file = fakeFile(new Blob(['some chunk data']))
    await attachChunkChecksum(req, file)
    expect(req.setHeader).toHaveBeenCalledWith('Upload-Checksum', expect.stringMatching(/^sha256 /))
  })

  it('does nothing for non-PATCH requests', async () => {
    const req = fakeRequest('POST', {})
    const file = fakeFile(new Blob(['data']))
    await attachChunkChecksum(req, file)
    expect(req.setHeader).not.toHaveBeenCalled()
  })

  it('computes the checksum only over the current chunk range', async () => {
    const content = 'a'.repeat(TUS_CHUNK_SIZE) + 'b'.repeat(100)
    const file = fakeFile(new Blob([content]))

    const reqFirstChunk = fakeRequest('PATCH', { 'Upload-Offset': '0' })
    await attachChunkChecksum(reqFirstChunk, file)
    const firstChecksum = (reqFirstChunk.setHeader as ReturnType<typeof vi.fn>).mock.calls[0][1]

    const reqSecondChunk = fakeRequest('PATCH', { 'Upload-Offset': String(TUS_CHUNK_SIZE) })
    await attachChunkChecksum(reqSecondChunk, file)
    const secondChecksum = (reqSecondChunk.setHeader as ReturnType<typeof vi.fn>).mock.calls[0][1]

    expect(firstChecksum).not.toBe(secondChecksum)
  })

  it('does nothing when no file data is present', async () => {
    const req = fakeRequest('PATCH', { 'Upload-Offset': '0' })
    const file = { data: undefined } as unknown as UppyFile<Record<string, unknown>, Record<string, unknown>>
    await attachChunkChecksum(req, file)
    expect(req.setHeader).not.toHaveBeenCalled()
  })
})
