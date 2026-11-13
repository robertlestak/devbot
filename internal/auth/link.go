package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"git.shdw.tech/rob/devbot/internal/audit"
)

// Linked sign-ins: a person's devbot sign-in tied to an identity somewhere
// else, a Slack user, instead of to a client holding devbot's tokens. Nobody
// is issued a token for one: what the identity does is resolved to the
// sign-in (Linked) by devbot itself, which already checked who sent it.
//
// A link starts from a single-use state bound to the identity (LinkURL).
// Whoever opens it first proves, in that browser, that they are the
// identity (a Prover: Sign in with Slack), and only then signs in through
// the forge's authorization code flow like any sign-in. Without the proof a
// link is phishable like the device flow: the identity sends its link to
// someone else, whose sign-in becomes theirs. The consent page names the
// identity. A link uses the person's one stored forge credential, so a
// forge that keeps one grant per person and app (Gitea) is refreshed the
// same way for every sign-in of theirs.

const (
	// linkClient is linked sign-ins' client. Registered clients' IDs all
	// start devbot_c_, so none can be it.
	linkClient = "devbot-link"
	// linkStateTTL is how long a link someone was offered can be started.
	linkStateTTL = 10 * time.Minute
	// linkProvedPath is where a link's proof of identity comes back to.
	linkProvedPath = "/oauth/link/callback"
)

// Link is an identity to link to a forge sign-in.
type Link struct {
	// Subject names the identity, unique across the install:
	// "slack:<enterprise>:<team>:<user>".
	Subject string `json:"subject"`
	// Group is what the link is removed with by UnlinkGroup: its Slack
	// workspace.
	Group string `json:"group"`
	// Who describes the identity to the person on the consent page.
	Who string `json:"who"`
	// Reply is the caller's own, handed back to the OnLink hook.
	Reply string `json:"reply,omitempty"`
}

// A Prover has the person following a link prove, in their browser, that
// they are the identity it is for: an OpenID Connect sign-in with the
// service the identity is of.
type Prover interface {
	// ProveURL is where to send the browser to prove l's identity. It
	// comes back to redirect with state and a code; nonce is bound into
	// what the code proves.
	ProveURL(l Link, redirect, state, nonce string) (string, error)
	// Proved is the subject, in Link.Subject's form, that whoever came
	// back with code proved they are. An error proves nobody.
	Proved(ctx context.Context, l Link, code, redirect, nonce string) (subject string, err error)
}

// ProveLinks has p prove the identity of every link whose subject is of
// kind, the part of it before the first colon. A link of a kind no Prover
// proves can't be made.
func (s *Server) ProveLinks(kind string, p Prover) {
	if s.provers == nil {
		s.provers = map[string]Prover{}
	}
	s.provers[kind] = p
}

func (s *Server) prover(subject string) Prover {
	kind, _, _ := strings.Cut(subject, ":")
	return s.provers[kind]
}

// linkProof is a link waiting for its identity to be proved, in the
// browser that opened it.
type linkProof struct {
	Link    Link   `json:"link"`
	Nonce   string `json:"nonce"`
	Browser string `json:"browser"` // hash of the sign-in cookie set in that browser
}

// linkRec is a standing link: the subject's sign-in.
type linkRec struct {
	Grant   string    `json:"grant"`
	Login   string    `json:"login"`
	Group   string    `json:"group"`
	Created time.Time `json:"created"`
}

// linkKey is a subject's link on this install's forge: one per subject and
// provider.
func (s *Server) linkKey(subject string) string { return "link:" + s.o.Forge.Name() + ":" + subject }

func linkGroupKey(group string) string { return prefix + "link-group:" + group }

// ErrNotLinked is a subject with no standing link.
var ErrNotLinked = errors.New("auth: not linked")

// OnLink adds f to what runs once a link is made, with the forge login it
// was made to. It is told, not asked: the link stands whatever f does.
func (s *Server) OnLink(f func(ctx context.Context, l Link, login string)) {
	s.onLink = append(s.onLink, f)
}

// LinkURL is a single-use URL that starts linking l, for linkStateTTL. The
// person who opens it proves they are l's identity, signs in through the
// forge, and is linked to l once they approve on the consent page, which
// names l.Who.
func (s *Server) LinkURL(ctx context.Context, l Link) (string, error) {
	if s.prover(l.Subject) == nil {
		return "", fmt.Errorf("auth: nothing proves the identity of a link to %q", l.Subject)
	}
	state := randomToken("")
	if err := s.set(ctx, "link-start:"+hash(state), l, linkStateTTL); err != nil {
		return "", err
	}
	return s.url("/oauth/link?state=" + state), nil
}

// linkStart takes a link's state, once, and sends the person to prove they
// are its identity, bound to their browser as a sign-in is.
func (s *Server) linkStart(w http.ResponseWriter, r *http.Request) {
	var l Link
	ok, err := s.getDel(r.Context(), "link-start:"+hash(r.URL.Query().Get("state")), &l)
	if err != nil {
		page(w, 500, "Something went wrong", "devbot couldn't read the link. Try again.")
		return
	}
	if !ok || l.Subject == "" {
		page(w, 400, "Link expired", "This link was already used or has expired. Ask devbot for a new one where you got it.")
		return
	}
	p := s.prover(l.Subject)
	if p == nil {
		page(w, 500, "Linking isn't set up", "devbot has no way to check who this link is for.")
		return
	}
	state, nonce, cookie := randomToken(""), randomToken(""), randomToken("")
	if err := s.set(r.Context(), "link-prove:"+hash(state), linkProof{Link: l, Nonce: nonce, Browser: hash(cookie)}, pendingTTL); err != nil {
		page(w, 500, "Something went wrong", "devbot couldn't start the link. Try again.")
		return
	}
	u, err := p.ProveURL(l, s.url(linkProvedPath), state, nonce)
	if err != nil {
		s.o.Log.Error("auth: starting a link's proof of identity", "err", err)
		page(w, 500, "Linking isn't set up", "devbot couldn't start checking who this link is for.")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: signinCookie, Value: cookie, Path: s.cookiePath("/oauth/"), MaxAge: int(pendingTTL.Seconds()),
		HttpOnly: true, Secure: strings.HasPrefix(s.o.ResourceURL, "https://"), SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, u, http.StatusFound)
}

// linkProved is where the person comes back from proving who they are. Only
// the identity the link was made for, proved in the browser that opened
// it, goes on to the forge: the forge step has no other way in.
func (s *Server) linkProved(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	var lp linkProof
	ok, err := s.getDel(ctx, "link-prove:"+hash(q.Get("state")), &lp)
	if err != nil {
		page(w, 500, "Something went wrong", "devbot couldn't read the link. Try again.")
		return
	}
	if !ok || lp.Link.Subject == "" {
		page(w, 400, "Link expired", "This link was already used or has expired. Ask devbot for a new one where you got it.")
		return
	}
	l := lp.Link
	if !sameBrowser(r, lp.Browser) {
		s.auditEvent(ctx, "auth.link", audit.Refused, "", linkClient, map[string]any{"reason": "proved in another browser", "link": l.Subject})
		page(w, 400, "Link started elsewhere", "This link was opened in another browser, so devbot won't finish it here. Ask devbot for a new one where you got it.")
		return
	}
	if q.Get("error") != "" {
		s.auditEvent(ctx, "auth.link", audit.Denied, "", linkClient, map[string]any{"reason": "the identity wasn't proved", "link": l.Subject})
		page(w, 403, "Not linked", "You didn't confirm who you are, so nothing was linked.")
		return
	}
	var proved string
	if p := s.prover(l.Subject); p == nil {
		err = errors.New("nothing proves this link's identity")
	} else {
		proved, err = p.Proved(ctx, l, q.Get("code"), s.url(linkProvedPath), lp.Nonce)
	}
	if err != nil {
		s.o.Log.Warn("auth: proving a link's identity", "link", l.Subject, "err", err)
		s.auditEvent(ctx, "auth.link", audit.Error, "", linkClient, map[string]any{"reason": "couldn't prove the identity", "link": l.Subject})
		page(w, 502, "Not linked", "devbot couldn't confirm who you are, so nothing was linked. Ask devbot for a new link where you got it.")
		return
	}
	if subtle.ConstantTimeCompare([]byte(proved), []byte(l.Subject)) != 1 {
		s.o.Log.Warn("auth: a link opened by someone else", "link", l.Subject, "proved", proved)
		s.auditEvent(ctx, "auth.link", audit.Refused, "", linkClient, map[string]any{"reason": "proved a different identity", "link": l.Subject, "proved": proved})
		page(w, 403, "This link is for someone else", "This link was made for "+l.Who+", but you confirmed you are someone else, so nothing was linked. Ask devbot for a link of your own.")
		return
	}
	s.auditEvent(ctx, "auth.link", audit.OK, "", linkClient, map[string]any{"reason": "identity proved", "link": l.Subject})
	s.toForge(w, r, pending{Client: linkClient, Link: &l, Verifier: oauth2.GenerateVerifier()})
}

// link makes the approved sign-in gid the subject's link, ending the one it
// replaces: one link per subject.
func (s *Server) link(ctx context.Context, gid, login string, l Link) error {
	unlock, err := s.lock(ctx, "lock:link:"+l.Subject)
	if err != nil {
		return err
	}
	var old linkRec
	had, err := s.get(ctx, s.linkKey(l.Subject), &old)
	if err == nil {
		_, err = s.keepLink(ctx, gid, l.Subject, linkRec{Grant: gid, Login: login, Group: l.Group, Created: s.o.Now().UTC()}, false)
	}
	unlock()
	if err != nil {
		return err
	}
	if had && old.Grant != gid {
		s.Revoke(ctx, old.Grant)
	}
	for _, f := range s.onLink {
		f(ctx, l, login)
	}
	return nil
}

// keepLink writes a link and its sign-in gid for another RefreshTTL: a link
// nobody uses for that long ends, like any sign-in. It holds the sign-in's
// lock, so one being revoked isn't written back. A sign-in that has ended
// is errGrant, and with linked set, so is one that isn't subject's.
func (s *Server) keepLink(ctx context.Context, gid, subject string, rec linkRec, linked bool) (*grant, error) {
	unlock, err := s.lock(ctx, gid)
	if err != nil {
		return nil, err
	}
	defer unlock()
	g, err := s.loadGrant(ctx, gid)
	if err != nil {
		return nil, err
	}
	if g == nil || g.Provider != s.o.Forge.Name() || (linked && g.Link != subject) {
		return nil, errGrant
	}
	g.Link = subject
	gb, _ := s.record("grant:"+g.ID, g)
	lb, err := s.record(s.linkKey(subject), rec)
	if err != nil {
		return nil, err
	}
	pipe := s.o.Redis.TxPipeline()
	pipe.Set(ctx, prefix+"grant:"+g.ID, gb, s.o.RefreshTTL)
	pipe.Set(ctx, prefix+s.linkKey(subject), lb, s.o.RefreshTTL)
	pipe.SAdd(ctx, linkGroupKey(rec.Group), subject)
	pipe.Expire(ctx, linkGroupKey(rec.Group), s.o.RefreshTTL)
	pipe.Expire(ctx, prefix+"forge:"+g.person(), s.credTTL())
	pipe.Expire(ctx, prefix+"person-grants:"+g.person(), s.credTTL())
	_, err = pipe.Exec(ctx)
	return g, err
}

// Linked is the sign-in subject is linked to, or ErrNotLinked. Each use
// keeps the link for another RefreshTTL. Pass it to User for the forge as
// that person, which ends the link too if the forge no longer accepts them.
func (s *Server) Linked(ctx context.Context, subject string) (Identity, error) {
	var rec linkRec
	ok, err := s.get(ctx, s.linkKey(subject), &rec)
	if err != nil {
		return Identity{}, err
	}
	if !ok {
		return Identity{}, ErrNotLinked
	}
	g, err := s.keepLink(ctx, rec.Grant, subject, rec, true)
	if errors.Is(err, errGrant) {
		// Its sign-in ended without taking the link along: a Redis that
		// lost one key and not the other.
		s.o.Redis.Del(ctx, prefix+s.linkKey(subject))
		return Identity{}, ErrNotLinked
	}
	if err != nil {
		return Identity{}, err
	}
	return Identity{Grant: g.ID, Provider: g.Provider, Login: g.Login, Client: g.Client}, nil
}

// Unlink ends subject's link and the sign-in behind it, and returns the
// forge login it was linked to. Nothing linked is ErrNotLinked, so it is
// safe to run again.
func (s *Server) Unlink(ctx context.Context, subject string) (string, error) {
	var rec linkRec
	ok, err := s.get(ctx, s.linkKey(subject), &rec)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", ErrNotLinked
	}
	if err := s.Revoke(ctx, rec.Grant); err != nil {
		return "", err
	}
	// Revoke took the link with the sign-in, unless that had already gone.
	return rec.Login, s.o.Redis.Del(ctx, prefix+s.linkKey(subject)).Err()
}

// UnlinkGroup ends every link in group, as Unlink does each.
func (s *Server) UnlinkGroup(ctx context.Context, group string) error {
	subjects, err := s.o.Redis.SMembers(ctx, linkGroupKey(group)).Result()
	if err != nil {
		return err
	}
	for _, sub := range subjects {
		if _, err := s.Unlink(ctx, sub); err != nil && !errors.Is(err, ErrNotLinked) {
			return err
		}
	}
	return s.o.Redis.Del(ctx, linkGroupKey(group)).Err()
}

// dropLink deletes g's link, for a caller ending g: a link goes with its
// sign-in, however that ends.
func (s *Server) dropLink(ctx context.Context, g *grant) {
	var rec linkRec
	if ok, _ := s.get(ctx, s.linkKey(g.Link), &rec); !ok || rec.Grant != g.ID {
		return
	}
	pipe := s.o.Redis.TxPipeline()
	pipe.Del(ctx, prefix+s.linkKey(g.Link))
	pipe.SRem(ctx, linkGroupKey(rec.Group), g.Link)
	pipe.Exec(ctx)
}
