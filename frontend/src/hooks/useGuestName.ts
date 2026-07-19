import { useCallback, useEffect, useState } from 'react'

const GUEST_NAME_KEY = 'wg_guest_name'

/**
 * Persists the guest's chosen display name in localStorage so it's
 * remembered across visits (and across the upload flow / like buttons)
 * without requiring any account system.
 */
export function useGuestName(): [string, (name: string) => void] {
  const [name, setNameState] = useState<string>(() => {
    try {
      return localStorage.getItem(GUEST_NAME_KEY) ?? ''
    } catch {
      return ''
    }
  })

  const setName = useCallback((next: string) => {
    setNameState(next)
    try {
      localStorage.setItem(GUEST_NAME_KEY, next)
    } catch {
      // ignore storage errors; name still works for this session via state
    }
  }, [])

  return [name, setName]
}

/** Reads the remembered guest name outside of a React component, e.g. for
 * attaching upload metadata in code that doesn't otherwise use the hook. */
export function readStoredGuestName(): string {
  try {
    return localStorage.getItem(GUEST_NAME_KEY) ?? ''
  } catch {
    return ''
  }
}

/** Hook that also tracks whether the guest has ever set a name, useful for
 * deciding whether to show a first-time prompt. */
export function useHasGuestName(): boolean {
  const [name] = useGuestName()
  const [has, setHas] = useState(() => name.trim().length > 0)
  useEffect(() => {
    setHas(name.trim().length > 0)
  }, [name])
  return has
}
