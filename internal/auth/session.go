package auth

import (
	"crypto/subtle"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"git.shdw.tech/rob/devbot/internal/audit"
	"git.shdw.tech/rob/devbot/internal/forge"
)

// The web UI's sign-in. A person signs in to devbot itself, through the
// forge like any client, but the browser keeps a session cookie instead of
// tokens: HttpOnly, Secure on https, SameSite=Lax and scoped to the UI's
// path. There is no consent page, since no third-party client asks to act
// as the person, and the UI only reads.

const (
	sessionCookie = "devbot_session"
	// sessionTTL is how long a UI sign-in lasts. The forge usually
	// remembers devbot's app, so signing in again is a redirect or two.
	sessionTTL = 12 * time.Hour
	// uiClient is the UI's grants' client. Registered clients' IDs all
	// start devbot_c_, so none can be it.
	uiClient = "devbot-ui"
)

// cookiePath is p under the path devbot is reached at, which a proxy may
// strip before serve sees the request.
func (s *Server) cookiePath(p string) string {
	u, err := url.Parse(s.o.ResourceURL)
	if err != nil {
		return p
	}
	return strings.TrimRight(u.Path, "/") + p
}

func (s *Server) secure() bool { return strings.HasPrefix(s.o.ResourceURL, "https://") }

// SignIn sends the browser to the forge to sign in to the web UI. Like every
// sign-in, it can only be finished in the browser that started it. It
// parks a sign-in in Redis as /oauth/authorize does, so it shares that
// endpoint's limit.
func (s *Server) SignIn(w http.ResponseWriter, r *http.Request) {
	s.limited("authorize", s.o.Limits.Authorize, true, "", func(w http.ResponseWriter, r *http.Request) {
		s.toForge(w, r, pending{Client: uiClient, UI: true, Verifier: oauth2.GenerateVerifier()})
	})(w, r)
}

// startSession finishes a UI sign-in: a grant for the person, and a session
// cookie that names it.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, login string, tok forge.UserToken) {
	ctx := r.Context()
	g, err := s.newGrant(ctx, uiClient, login, tok, false)
	if err != nil {
		page(w, 500, "Something went wrong", "devbot couldn't record the sign-in. Try again.")
		return
	}
	st := randomToken("devbot_session_")
	g.Session = hash(st)
	err = s.set(ctx, "session:"+g.Session, accessRec{Grant: g.ID, Audience: s.url("/ui"), Expiry: s.o.Now().Add(sessionTTL)}, sessionTTL)
	if err == nil {
		err = s.set(ctx, "grant:"+g.ID, g, sessionTTL)
	}
	if err != nil {
		page(w, 500, "Something went wrong", "devbot couldn't record the sign-in. Try again.")
		return
	}
	s.o.Log.Info("auth: signed in", "login", login, "provider", s.o.Forge.Name(), "client", uiClient)
	s.auditEvent(ctx, "auth.sign_in", audit.OK, login, uiClient, map[string]any{"ui": true})
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: st, Path: s.cookiePath("/ui/"), MaxAge: int(sessionTTL.Seconds()),
		HttpOnly: true, Secure: s.secure(), SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, s.url("/ui/"), http.StatusFound)
}

// Session resolves a web UI request's session cookie. Anything else, a
// bearer token included, is ErrInvalidToken; and a session is no bearer
// token, so it opens nothing outside the UI.
func (s *Server) Session(r *http.Request) (Identity, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return Identity{}, ErrInvalidToken
	}
	ctx := r.Context()
	var rec accessRec
	found, err := s.get(ctx, "session:"+hash(c.Value), &rec)
	if err != nil {
		return Identity{}, err
	}
	if !found || rec.Audience != s.url("/ui") || !s.o.Now().Before(rec.Expiry) {
		return Identity{}, ErrInvalidToken
	}
	g, err := s.loadGrant(ctx, rec.Grant)
	if err != nil {
		return Identity{}, err
	}
	if g == nil || g.Session != hash(c.Value) || g.Provider != s.o.Forge.Name() {
		return Identity{}, ErrInvalidToken
	}
	return Identity{Grant: g.ID, Provider: g.Provider, Login: g.Login, Client: g.Client}, nil
}

// CSRF is the token the UI's forms carry, derived from the session cookie,
// which a page on another site can't read.
func (s *Server) CSRF(r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return ""
	}
	return hash("csrf|" + c.Value)
}

// SignOut ends the UI session's grant. Only a same-origin post with the
// session's CSRF token counts.
func (s *Server) SignOut(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	r.ParseForm()
	want := s.CSRF(r)
	if !sameOrigin(r, s.o.ResourceURL) || want == "" || subtle.ConstantTimeCompare([]byte(r.PostForm.Get("csrf")), []byte(want)) != 1 {
		page(w, 403, "Not allowed", "Sign out from devbot's own page.")
		return
	}
	if id, err := s.Session(r); err == nil {
		if err := s.Revoke(r.Context(), id.Grant); err != nil {
			page(w, 500, "Something went wrong", "devbot couldn't end the session. Try again.")
			return
		}
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: s.cookiePath("/ui/"), MaxAge: -1,
		HttpOnly: true, Secure: s.secure(), SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, s.url("/ui/"), http.StatusSeeOther)
}
