import { useEffect, useMemo, useRef, useState } from 'react'
import Uppy from '@uppy/core'
import Tus from '@uppy/tus'
import DashboardModal from '@uppy/react/dashboard-modal'
import { CloudUpload } from 'lucide-react'

import '@uppy/core/css/style.min.css'
import '@uppy/dashboard/css/style.min.css'

import { sha256OfFile } from '../utils/hash'
import { formatBrandingText } from '../utils/branding'
import { checkUploadDuplicate } from '../api/client'
import type { BrandingConfig, PublicConfig } from '../types'

// tus request (chunk) size. 8 MiB keeps each PATCH safely under common
// reverse-proxy body limits (e.g. Cloudflare's 100 MB cap) while still
// letting tus resume from the last completed chunk after an interruption.
const TUS_CHUNK_SIZE = 8 * 1024 * 1024

interface UploadPanelProps {
  guestName: string
  config: PublicConfig
  branding: BrandingConfig
  onUploadComplete: (successfulUploads: number) => void
}

/**
 * Guest upload UI built on Uppy + tus-js-client (via @uppy/tus). Handles:
 *  - a client-side whole-file SHA-256 preflight to skip re-uploading files
 *    already in the gallery (server re-verifies authoritatively too),
 *  - resumable, chunked uploads safely under common reverse-proxy body size
 *    limits (8 MiB chunks, well under Cloudflare's 100 MB request cap), with
 *    automatic retry of failed chunks and resume of interrupted uploads.
 *
 * The upload queue lives in Uppy Dashboard's battle-tested modal, opened by
 * a compact trigger so it does not displace the gallery on small screens.
 * Byte integrity in transit is provided by HTTPS (TLS), so no application-
 * level per-chunk checksum is used; the whole-file hash above is still
 * re-verified server-side before the file is stored.
 */
export function UploadPanel({ guestName, config, branding, onUploadComplete }: UploadPanelProps) {
  const [modalOpen, setModalOpen] = useState(false)
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

    instance.on('complete', (result) => {
      const successfulUploads = result.successful?.length ?? 0
      if (successfulUploads > 0) onUploadCompleteRef.current(successfulUploads)
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
    return <p className="upload-closed">{branding.uploadsClosedText}</p>
  }

  const maxSizeGiB = config.maxUploadBytes / (1024 * 1024 * 1024)
  const maxSizeLabel = maxSizeGiB >= 1 ? `${Number(maxSizeGiB.toFixed(1))} GB` : `${Math.floor(config.maxUploadBytes / (1024 * 1024))} MB`
  const helperText = formatBrandingText(branding.uploadHelperText, { maxSize: maxSizeLabel })

  return (
    <>
      <button
        type="button"
        className="upload-trigger"
        onClick={() => setModalOpen(true)}
        aria-label={branding.uploadButtonText || 'Add photos and videos'}
      >
        <span className="upload-trigger-icon" aria-hidden="true">
          <CloudUpload size={22} strokeWidth={1.8} />
        </span>
        <span className="upload-trigger-copy">
          {branding.uploadButtonText && <strong>{branding.uploadButtonText}</strong>}
          {helperText && <span>{helperText}</span>}
        </span>
      </button>

      <DashboardModal
        uppy={uppy}
        open={modalOpen}
        onRequestClose={() => setModalOpen(false)}
        closeAfterFinish
        closeModalOnClickOutside
        proudlyDisplayPoweredByUppy={false}
        note={helperText}
      />
    </>
  )
}
