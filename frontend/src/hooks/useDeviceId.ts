const DEVICE_ID_KEY = 'eg_device_id'

/**
 * Returns a stable, random per-device identifier persisted in
 * localStorage. Used to deduplicate "likes" per device (so refreshing the
 * page or another guest on a shared link can't inflate the like count) and
 * to let the gallery report which items *this* device has already liked.
 *
 * This is NOT an authentication mechanism -- it's just enough to keep the
 * like feature honest for casual use at an event, not to resist a
 * determined adversary.
 */
export function getDeviceId(): string {
  try {
    let id = localStorage.getItem(DEVICE_ID_KEY)
    if (!id) {
      id = crypto.randomUUID()
      localStorage.setItem(DEVICE_ID_KEY, id)
    }
    return id
  } catch {
    // localStorage unavailable (e.g. private browsing in some browsers):
    // fall back to a per-session id so likes at least work within the tab.
    return sessionDeviceId()
  }
}

let sessionFallbackId: string | null = null
function sessionDeviceId(): string {
  if (!sessionFallbackId) {
    sessionFallbackId = crypto.randomUUID()
  }
  return sessionFallbackId
}
