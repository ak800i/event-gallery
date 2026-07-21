package httpapi

import (
	"context"
	"net/http"

	"event-gallery/backend/internal/store"
)

const (
	sessionCookieName = "wg_session"
	csrfCookieName    = "wg_csrf"
	csrfHeaderName    = "X-CSRF-Token"
)

type contextKey string

const sessionContextKey contextKey = "admin_session"

func sessionFromContext(ctx context.Context) *store.AdminSession {
	sess, _ := ctx.Value(sessionContextKey).(*store.AdminSession)
	return sess
}

// setSessionCookies attaches the session and CSRF cookies to the response
// after a successful admin login. The session cookie is HttpOnly so it
// cannot be read by JavaScript (mitigating XSS token theft); the CSRF
// cookie deliberately is not, since the SPA must read it and echo it back
// in a request header (double-submit pattern) to prove the request
// originated from same-origin JavaScript rather than a cross-site form.
func setSessionCookies(w http.ResponseWriter, sess *store.AdminSession, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sess.ID,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		Expires:  sess.ExpiresAt,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    sess.CSRFToken,
		Path:     "/",
		HttpOnly: false,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		Expires:  sess.ExpiresAt,
	})
}

func clearSessionCookies(w http.ResponseWriter, secure bool) {
	for _, name := range []string{sessionCookieName, csrfCookieName} {
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    "",
			Path:     "/",
			HttpOnly: name == sessionCookieName,
			Secure:   secure,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   -1,
		})
	}
}

// requireAdmin loads and validates the session cookie, rejecting the
// request with 401 if missing/expired, and otherwise attaches the session
// to the request context for downstream handlers (and requireCSRF).
func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil || cookie.Value == "" {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		sess, err := s.store.GetSession(r.Context(), cookie.Value)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "session expired or invalid")
			return
		}
		ctx := context.WithValue(r.Context(), sessionContextKey, sess)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requireCSRF enforces the double-submit CSRF protection for
// state-changing admin requests: the X-CSRF-Token header must match the
// token bound to the caller's authenticated session. Must run after
// requireAdmin in the middleware chain.
func (s *Server) requireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFromContext(r.Context())
		if sess == nil {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		token := r.Header.Get(csrfHeaderName)
		if token == "" || token != sess.CSRFToken {
			writeError(w, http.StatusForbidden, "invalid or missing CSRF token")
			return
		}
		next.ServeHTTP(w, r)
	})
}
