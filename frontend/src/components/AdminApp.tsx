import { useEffect, useState } from 'react'
import type { FormEvent } from 'react'
import { adminCheckSession, adminLogin } from '../api/client'
import { AdminDashboard } from './AdminDashboard'

/** Root of the admin area: shows a password-only login form (there is
 * intentionally no username -- see ADMIN_PASSWORD in the README) until an
 * authenticated session is established, then renders the dashboard. */
export function AdminApp() {
  const [checking, setChecking] = useState(true)
  const [authenticated, setAuthenticated] = useState(false)
  const [password, setPassword] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [submitting, setSubmitting] = useState(false)

  useEffect(() => {
    adminCheckSession()
      .then(setAuthenticated)
      .finally(() => setChecking(false))
  }, [])

  async function handleSubmit(e: FormEvent) {
    e.preventDefault()
    setSubmitting(true)
    setError(null)
    try {
      await adminLogin(password)
      setAuthenticated(true)
    } catch {
      setError('Incorrect password.')
    } finally {
      setSubmitting(false)
    }
  }

  if (checking) return <p>Loading...</p>

  if (!authenticated) {
    return (
      <div className="admin-login">
        <h1>Admin sign in</h1>
        <form onSubmit={handleSubmit}>
          <label htmlFor="admin-password">Password</label>
          <input
            id="admin-password"
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            autoFocus
            required
          />
          {error && <p className="form-error">{error}</p>}
          <button type="submit" disabled={submitting}>
            {submitting ? 'Signing in...' : 'Sign in'}
          </button>
        </form>
      </div>
    )
  }

  return <AdminDashboard onLoggedOut={() => setAuthenticated(false)} />
}
