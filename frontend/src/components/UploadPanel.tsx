import { useEffect, useMemo, useRef } from 'react'
import Uppy from '@uppy/core'
import Tus from '@uppy/tus'
import Webcam from '@uppy/webcam'
import Dashboard from '@uppy/react/dashboard'

import '@uppy/core/css/style.min.css'
import '@uppy/dashboard/css/style.min.css'
import '@uppy/webcam/css/style.min.css'

import { sha256OfFile } from '../utils/hash'
import { checkUploadDuplicate } from '../api/client'
import type { PublicConfig } from '../types'

// tus request (chunk) size. 8 MiB keeps each PATCH safely under common
// reverse-proxy body limits (e.g. Cloudflare's 100 MB cap) while still
// letting tus resume from the last completed chunk after an interruption.
const TUS_CHUNK_SIZE = 8 * 1024 * 1024

interface UploadPanelProps {
  guestName: string
  config: PublicConfig
  onUploadComplete: () => void
}

/**
 * Guest upload UI built on Uppy + tus-js-client (via @uppy/tus). Handles:
 *  - a client-side whole-file SHA-256 preflight to skip re-uploading files
 *    already in the gallery (server re-verifies authoritatively too),
 *  - resumable, chunked uploads safely under common reverse-proxy body size
 *    limits (8 MiB chunks, well under Cloudflare's 100 MB request cap), with
 *    automatic retry of failed chunks and resume of interrupted uploads,
 *  - camera capture on mobile via the Webcam plugin.
 *
 * Byte integrity in transit is provided by HTTPS (TLS), so no application-
 * level per-chunk checksum is used; the whole-file hash above is still
 * re-verified server-side before the file is stored.
 */
export function UploadPanel({ guestName, config, onUploadComplete }: UploadPanelProps) {
  const onUploadCompleteRef = useRef(onUploadComplete)
  useEffect(() => {
    onUploadCompleteRef.current = onUploadComplete
  }, [onUploadComplete])

  const uppy = useMemo(() => {
    const instance = new Uppy({
      restrictions: {
        maxFileSize: config.maxUploadBytes,
        allowedFileTypes: [...config.allowedImageMimeTypes, ...config.allowedVideoMimeTypes],
      },
      autoProceed: false,
    })

    instance.use(Tus, {
      endpoint: '/api/tus/',
      chunkSize: TUS_CHUNK_SIZE,
      limit: config.uploadConcurrency,
      retryDelays: [0, 1000, 3000, 5000, 10000],
      removeFingerprintOnSuccess: true,
    })

    instance.use(Webcam, { modes: ['picture', 'video-audio'] })

    instance.addPreProcessor(async (fileIDs: string[]) => {
      await Promise.all(
        fileIDs.map(async (id) => {
          const file = instance.getFile(id)
          if (!file || !file.data) return
          try {
            const sha256 = await sha256OfFile(file.data as Blob)
            const check = await checkUploadDuplicate(sha256, file.size ?? 0, file.name ?? 'upload')
            if (check.duplicate) {
              instance.info(`"${file.name}" is already in the gallery -- skipping upload.`, 'warning', 5000)
              instance.removeFile(id)
              return
            }
            instance.setFileMeta(id, { sha256 })
          } catch {
            instance.info(`Could not prepare "${file.name}" for upload.`, 'error', 5000)
            instance.removeFile(id)
          }
        }),
      )
    })

    instance.on('complete', () => {
      onUploadCompleteRef.current()
    })

    return instance
  }, [config.maxUploadBytes, config.uploadConcurrency, config.allowedImageMimeTypes, config.allowedVideoMimeTypes])

  useEffect(() => {
    uppy.setMeta({ guestName })
  }, [uppy, guestName])

  useEffect(() => {
    return () => {
      uppy.destroy()
    }
  }, [uppy])

  if (!config.uploadsEnabled) {
    return <p className="upload-closed">Uploads are closed for this gallery.</p>
  }

  return (
    <Dashboard
      uppy={uppy}
      proudlyDisplayPoweredByUppy={false}
      height={380}
      note={`Photos & videos up to ${Math.floor(config.maxUploadBytes / (1024 * 1024))} MB`}
    />
  )
}
