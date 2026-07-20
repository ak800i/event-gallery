import { render } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import type { MediaItem } from '../types'
import { Lightbox } from './Lightbox'

const lightboxSpy = vi.hoisted(() => vi.fn())

vi.mock('yet-another-react-lightbox', () => ({
  default: (props: unknown) => {
    lightboxSpy(props)
    return <div role="dialog" aria-label="Lightbox" />
  },
}))

vi.mock('yet-another-react-lightbox/plugins/video', () => ({ default: vi.fn() }))

function item(overrides: Partial<MediaItem>): MediaItem {
  return {
    id: 'image-id',
    originalFilename: 'photo.jpg',
    kind: 'image',
    mimeType: 'image/jpeg',
    sizeBytes: 1024,
    width: 1600,
    height: 1200,
    hasThumbnail: true,
    uploadedAt: '2026-01-01T00:00:00Z',
    uploaderName: 'Alex',
    likeCount: 0,
    likedByDevice: false,
    ...overrides,
  }
}

interface CapturedLightboxProps {
  slides: Array<Record<string, unknown>>
  carousel: { finite: boolean; preload: number }
  controller: { aria: boolean; closeOnBackdropClick: boolean }
  video: { controls: boolean; playsInline: boolean; preload: string }
}

describe('Lightbox media mapping', () => {
  it('maps image and video items into bounded-preload YARL slides', () => {
    render(
      <Lightbox
        items={[
          item({}),
          item({ id: 'video-id', originalFilename: 'clip.mp4', kind: 'video', mimeType: 'video/mp4' }),
        ]}
        index={0}
        onClose={vi.fn()}
        onIndexChange={vi.fn()}
      />,
    )

    const props = lightboxSpy.mock.calls.at(-1)?.[0] as CapturedLightboxProps
    expect(props.carousel).toMatchObject({ finite: true, preload: 1 })
    expect(props.controller).toEqual({ aria: true, closeOnBackdropClick: true })
    expect(props.slides[0]).toMatchObject({
      type: 'image',
      src: '/api/media/image-id/file',
      alt: 'photo.jpg',
      width: 1600,
      height: 1200,
    })
    expect(props.slides[1]).toMatchObject({
      type: 'video',
      poster: '/api/media/video-id/thumbnail',
      controls: true,
      playsInline: true,
      sources: [{ src: '/api/media/video-id/file', type: 'video/mp4' }],
    })
    expect(props.video).toEqual({ controls: true, playsInline: true, preload: 'metadata' })
  })
})
