// Package auth is devbot's OAuth broker for its connectors (auth.mode oauth),
// the MCP authorization spec's third-party flow. devbot is the authorization
// server its MCP and API clients see, and itself an OAuth client of one forge,
// which is the identity provider. Clients only ever hold devbot's own tokens,
// bound to devbot's resource URL: a forge token is never accepted from a
// client (that would be token passthrough) and never handed to one.
//
// Tokens are opaque random strings. Redis keeps only their SHA-256, with the
// grant they belong to, so every request is a lookup that checks expiry,
// audience and that the grant still stands. A grant is one sign-in: a person
// on the forge and the client they signed in from. Deleting it revokes
// everything issued for it at once.
//
// The person's forge token is stored once, not per grant, sealed with the
// install's key and shared by all of their grants: Gitea keeps one OAuth
// grant per user and app, and each refresh or new sign-in there invalidates
// the refresh tokens before it, so per-grant forge tokens would sign each
// other out.
//
// Like the rest of devbot's Redis state it is disposable: after a wipe,
// clients get a 401 and sign in again.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"git.shdw.tech/rob/devbot/internal/audit"
	"git.shdw.tech/rob/devbot/internal/forge"
)

// ErrInvalidToken is a request without a usable devbot access token: none,
// unknown, expired, for another audience, or from a grant that has ended.
// Callers answer it with a 401 (Challenge).
var ErrInvalidToken = errors.New("invalid or missing access token")

const (
	prefix     = "devbot:auth:"
	pendingTTL = 10 * time.Minute // a person's time at the forge's login page
	codeTTL    = 2 * time.Minute
	// A redeemed code is remembered this long: presented again, it ends
	// the grant it produced.
	usedCodeTTL = 10 * time.Minute
	clientTTL   = 24 * time.Hour // an unused dynamic registration
	deviceTTL   = 10 * time.Minute
	// A forge token this close to expiry is refreshed before use.
	refreshEarly = time.Minute
)

type Options struct {
	// ResourceURL is devbot's public base URL: the issuer, the audience of
	// every token, and the base of the endpoints.
	ResourceURL string
	// Forge is the identity provider.
	Forge forge.Provider
	Redis *redis.Client
	// Key encrypts forge tokens at rest; PreviousKey, when set, still
	// decrypts during a rotation.
	Key, PreviousKey []byte
	AccessTTL        time.Duration
	RefreshTTL       time.Duration
	// RefreshReuseGrace is how long a rotated refresh token, presented
	// again by the same client, gets back the tokens it was rotated into
	// instead of ending the grant: processes sharing one stored sign-in
	// refresh at about the same time. Zero turns it off.
	RefreshReuseGrace time.Duration
	// DeviceGrant lets dynamically registered clients, which every client
	// is, use the device grant. Off by default: a device code is easy to
	// phish (RFC 8628 5.4).
	DeviceGrant bool
	// Limits are the per-address request limits.
	Limits Limits
	// TrustedProxies are the proxies whose X-Forwarded-For names the
	// client; a request from any other peer is that peer's address.
	TrustedProxies []netip.Prefix
	Log            *slog.Logger
	// Audit records sign-ins, token issues and refreshes, revocations and
	// client registrations. Nil records nothing.
	Audit *audit.Logger
	// Now is the clock, for tests.
	Now func() time.Time
}

type Server struct {
	o    Options
	seal *sealer
	lim  limiter
}

func New(o Options) (*Server, error) {
	if o.ResourceURL == "" || o.Forge == nil || o.Redis == nil {
		return nil, errors.New("auth: resource URL, forge and Redis are required")
	}
	o.ResourceURL = strings.TrimRight(o.ResourceURL, "/")
	sl, err := newSealer(o.Key, o.PreviousKey)
	if err != nil {
		return nil, err
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	return &Server{o: o, seal: sl}, nil
}

// Identity is who an authenticated request is for.
type Identity struct {
	Grant    string
	Provider string // the forge they signed in to
	Login    string // their login there
	Client   string // the OAuth client they signed in from
}

// ---- records ----

type client struct {
	ID           string   `json:"client_id"`
	SecretHash   string   `json:"secret_hash,omitempty"`
	Name         string   `json:"client_name,omitempty"`
	RedirectURIs []string `json:"redirect_uris,omitempty"`
	GrantTypes   []string `json:"grant_types"`
	AuthMethod   string   `json:"token_endpoint_auth_method"`
	IssuedAt     int64    `json:"client_id_issued_at"`
}

func (c *client) allows(grantType string) bool {
	for _, g := range c.GrantTypes {
		if g == grantType {
			return true
		}
	}
	return false
}

// pending is an authorization in progress while the person is at the forge.
type pending struct {
	Client      string `json:"client"`
	RedirectURI string `json:"redirect_uri,omitempty"` // as the client sent it
	Redirect    string `json:"redirect,omitempty"`     // where to send the code
	State       string `json:"state,omitempty"`
	Challenge   string `json:"challenge,omitempty"`
	Device      string `json:"device,omitempty"` // device code hash, for the device flow
	UI          bool   `json:"ui,omitempty"`     // a sign-in to the web UI, which ends in a session cookie
	Browser     string `json:"browser"`          // hash of the sign-in cookie set in the browser that started it
	Verifier    string `json:"verifier"`         // devbot's own PKCE verifier toward the forge
}

type codeRec struct {
	Client      string `json:"client"`
	RedirectURI string `json:"redirect_uri,omitempty"`
	Challenge   string `json:"challenge"`
	Grant       string `json:"grant"`
}

type grant struct {
	ID       string    `json:"id"`
	Client   string    `json:"client"`
	Provider string    `json:"provider"`
	Login    string    `json:"login"`
	Access   string    `json:"access,omitempty"`  // hash of the current access token
	Refresh  string    `json:"refresh,omitempty"` // hash of the current refresh token
	Device   bool      `json:"device,omitempty"`  // signed in through the device flow
	Session  string    `json:"session,omitempty"` // hash of the web UI session token
	Retired  string    `json:"retired,omitempty"` // hash of the refresh token rotated out last, which may have a grace record
	Created  time.Time `json:"created"`
}

type accessRec struct {
	Grant    string    `json:"grant"`
	Audience string    `json:"aud"`
	Expiry   time.Time `json:"exp"`
}

type deviceRec struct {
	Client   string    `json:"client"`
	UserCode string    `json:"user_code"`
	Grant    string    `json:"grant,omitempty"` // set once the person signed in
	Denied   bool      `json:"denied,omitempty"`
	LastPoll time.Time `json:"last_poll,omitzero"`
}

func randomToken(prefix string) string {
	b := make([]byte, 32)
	rand.Read(b)
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

// hash is how a token or code is stored: whoever reads Redis can't use it.
func hash(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

// record is v as it's stored under key: JSON with a MAC (sealer.sign).
// Every JSON record is, so writing to Redis doesn't make a grant, a token or
// a code: only serve, holding the auth key, can.
func (s *Server) record(key string, v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return s.seal.sign(prefix+key, b), nil
}

// unpack decodes a record read from key into v. One whose MAC doesn't hold
// is as good as absent. One signed with the previous key is signed again
// under the current one, unless it has changed since: a record in use
// through a rotation outlives it, as forge tokens do.
func (s *Server) unpack(ctx context.Context, key string, b []byte, v any) (bool, error) {
	rec, stale, ok := s.seal.verify(prefix+key, b)
	if !ok {
		kind, _, _ := strings.Cut(key, ":")
		s.o.Log.Warn("auth: a record in Redis fails its MAC; ignoring it", "kind", kind)
		return false, nil
	}
	if stale {
		resignScript.Run(ctx, s.o.Redis, []string{prefix + key}, b, s.seal.sign(prefix+key, rec))
	}
	return true, json.Unmarshal(rec, v)
}

var resignScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('SET', KEYS[1], ARGV[2], 'KEEPTTL') end
return 0`)

func (s *Server) get(ctx context.Context, key string, v any) (bool, error) {
	b, err := s.o.Redis.Get(ctx, prefix+key).Bytes()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return s.unpack(ctx, key, b, v)
}

func (s *Server) getDel(ctx context.Context, key string, v any) (bool, error) {
	b, err := s.o.Redis.GetDel(ctx, prefix+key).Bytes()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return s.unpack(ctx, key, b, v)
}

func (s *Server) set(ctx context.Context, key string, v any, ttl time.Duration) error {
	b, err := s.record(key, v)
	if err != nil {
		return err
	}
	return s.o.Redis.Set(ctx, prefix+key, b, ttl).Err()
}

// keepTTL rewrites a record without changing when it expires.
func (s *Server) keepTTL(ctx context.Context, key string, v any) error {
	b, err := s.record(key, v)
	if err != nil {
		return err
	}
	return s.o.Redis.SetArgs(ctx, prefix+key, b, redis.SetArgs{KeepTTL: true, Mode: "XX"}).Err()
}

func (s *Server) loadClient(ctx context.Context, id string) (*client, error) {
	if id == "" {
		return nil, nil
	}
	var c client
	ok, err := s.get(ctx, "client:"+id, &c)
	if !ok || err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *Server) loadGrant(ctx context.Context, id string) (*grant, error) {
	var g grant
	ok, err := s.get(ctx, "grant:"+id, &g)
	if !ok || err != nil {
		return nil, err
	}
	return &g, nil
}

// lock serializes changes to one record: "lock:"+gid for a grant, whose
// tokens issue rewrites, and personLock for a person's forge credential, which
// a forge that rotates refresh tokens would end if two refreshes raced. A
// holder of a person's lock never takes a grant's.
func (s *Server) lock(ctx context.Context, name string) (func(), error) {
	key, owner := prefix+name, randomToken("")
	for i := 0; ; i++ {
		ok, err := s.o.Redis.SetNX(ctx, key, owner, 30*time.Second).Result()
		if err != nil {
			return nil, err
		}
		if ok {
			return func() {
				s.o.Redis.Eval(context.WithoutCancel(ctx), `if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end return 0`, []string{key}, owner)
			}, nil
		}
		if i >= 200 {
			return nil, errors.New("auth: " + name + " is held")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// ---- grants and tokens ----

// newGrant records a sign-in. It lives only as long as a code or device
// approval until tokens are issued. The forge token it came with replaces the
// person's stored one: the forge has just moved its grant on, so the newest
// token is the only one sure to still refresh.
func (s *Server) newGrant(ctx context.Context, clientID, login string, tok forge.UserToken, device bool) (*grant, error) {
	g := &grant{ID: randomToken("g"), Client: clientID, Provider: s.o.Forge.Name(), Login: login, Device: device, Created: s.o.Now().UTC()}
	p := g.person()
	unlock, err := s.lock(ctx, personLock(p))
	if err != nil {
		return nil, err
	}
	defer unlock()
	gb, _ := s.record("grant:"+g.ID, g)
	pipe := s.o.Redis.TxPipeline()
	pipe.Set(ctx, prefix+"grant:"+g.ID, gb, pendingTTL)
	pipe.SAdd(ctx, prefix+"person-grants:"+p, g.ID)
	pipe.Expire(ctx, prefix+"person-grants:"+p, s.credTTL())
	s.writeCred(ctx, pipe, p, tok)
	_, err = pipe.Exec(ctx)
	return g, err
}

// ---- the shared forge credential ----

// person names whose forge credential a grant uses: the forge and the
// person's login there, which the forge treats case-insensitively.
func (g *grant) person() string { return g.Provider + ":" + strings.ToLower(g.Login) }

func personLock(p string) string { return "lock:person:" + p }

// credAAD binds a sealed forge token to its person, so a record can't be
// moved to another key.
func credAAD(p string) string { return "forge\x00" + p }

// credTTL outlasts any grant written now, so a credential is still there
// for every grant that uses it. issue extends it with each grant.
func (s *Server) credTTL() time.Duration { return max(s.o.RefreshTTL, sessionTTL) }

func (s *Server) writeCred(ctx context.Context, pipe redis.Pipeliner, p string, tok forge.UserToken) {
	b, _ := json.Marshal(tok)
	pipe.Set(ctx, prefix+"forge:"+p, s.seal.seal(b, credAAD(p)), s.credTTL())
}

// readCred returns a person's forge token, and whether it needs re-sealing
// under the current key. A missing one is ErrInvalidToken; one that won't
// open, errSealed.
func (s *Server) readCred(ctx context.Context, p string) (forge.UserToken, bool, error) {
	sealed, err := s.o.Redis.Get(ctx, prefix+"forge:"+p).Result()
	if errors.Is(err, redis.Nil) {
		return forge.UserToken{}, false, ErrInvalidToken
	}
	if err != nil {
		return forge.UserToken{}, false, err
	}
	b, stale, err := s.seal.open(sealed, credAAD(p))
	if err != nil {
		// Sealed under a key the install no longer has: sign in again.
		s.o.Log.Warn("auth: can't open a stored forge token; ending the person's access", "person", p, "err", err)
		return forge.UserToken{}, false, errSealed
	}
	var t forge.UserToken
	return t, stale, json.Unmarshal(b, &t)
}

// dropCred deletes a person's forge credential, for a caller holding their
// lock, when the forge no longer accepts it: each of their grants then ends
// on its next use. With access set, only while that is still the stored
// access token, so a rejection of a token already replaced drops nothing.
func (s *Server) dropCred(ctx context.Context, p, access string) {
	if access != "" {
		if t, _, err := s.readCred(ctx, p); err != nil || t.Access != access {
			return
		}
	}
	s.o.Redis.Del(ctx, prefix+"forge:"+p)
}

// releaseCred takes gid off the person's grants, deleting their forge
// credential when no grant of theirs is left. Grants that expired on their
// own are pruned on the way.
func (s *Server) releaseCred(ctx context.Context, p, gid string) error {
	unlock, err := s.lock(ctx, personLock(p))
	if err != nil {
		return err
	}
	defer unlock()
	set := prefix + "person-grants:" + p
	if err := s.o.Redis.SRem(ctx, set, gid).Err(); err != nil {
		return err
	}
	ids, err := s.o.Redis.SMembers(ctx, set).Result()
	if err != nil {
		return err
	}
	left := 0
	for _, id := range ids {
		n, err := s.o.Redis.Exists(ctx, prefix+"grant:"+id).Result()
		if err != nil {
			return err
		}
		if n == 0 {
			s.o.Redis.SRem(ctx, set, id)
			continue
		}
		left++
	}
	if left == 0 {
		return s.o.Redis.Del(ctx, set, prefix+"forge:"+p).Err()
	}
	return nil
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
}

// issue gives a grant a new access and refresh token, retiring its old ones:
// refresh tokens rotate on every use. With refresh set (the hash of the
// token being redeemed), it only issues while that is still the grant's
// current refresh token, so two redemptions of one token can't both mint
// tokens: within the grace window the later one gets the earlier one's.
func (s *Server) issue(ctx context.Context, gid, refresh string) (tokenResponse, error) {
	unlock, err := s.lock(ctx, gid)
	if err != nil {
		return tokenResponse{}, err
	}
	defer unlock()
	g, err := s.loadGrant(ctx, gid)
	if err != nil {
		return tokenResponse{}, err
	}
	if g == nil {
		return tokenResponse{}, errGrant
	}
	if refresh != "" && g.Refresh != refresh {
		// Lost a race with a refresh of the same token, or came after it
		// within the grace window: get what that one got.
		resp, err := s.replay(ctx, g, refresh)
		if errors.Is(err, errReused) {
			s.o.Log.Warn("auth: a rotated refresh token was reused; ending the grant")
			s.endGrant(ctx, g.ID)
		}
		return resp, err
	}
	at, rt := randomToken("devbot_at_"), randomToken("devbot_rt_")
	exp := s.o.Now().Add(s.o.AccessTTL)
	pipe := s.o.Redis.TxPipeline()
	if g.Access != "" {
		pipe.Del(ctx, prefix+"at:"+g.Access)
	}
	if g.Retired != "" {
		// The tokens it was rotated into are being retired too, so its
		// grace is over.
		pipe.Del(ctx, prefix+"rt-grace:"+g.Retired)
		g.Retired = ""
	}
	if g.Refresh != "" {
		pipe.Del(ctx, prefix+"rt:"+g.Refresh)
		// Remember retired refresh tokens: one presented again means it
		// leaked, and the grant ends.
		pipe.Set(ctx, prefix+"rt-used:"+g.Refresh, g.ID, s.o.RefreshTTL)
		if refresh != "" && s.o.RefreshReuseGrace > 0 {
			b, _ := json.Marshal(graceTokens{Access: at, Refresh: rt, Expiry: exp})
			gr, _ := s.record("rt-grace:"+refresh, graceRec{Grant: g.ID, Until: s.o.Now().Add(s.o.RefreshReuseGrace), Tokens: s.seal.seal(b, graceAAD(g.ID, refresh))})
			pipe.Set(ctx, prefix+"rt-grace:"+refresh, gr, s.o.RefreshReuseGrace)
			g.Retired = refresh
		}
	}
	g.Access, g.Refresh = hash(at), hash(rt)
	gb, _ := s.record("grant:"+g.ID, g)
	rec, _ := s.record("at:"+g.Access, accessRec{Grant: g.ID, Audience: s.o.ResourceURL, Expiry: exp})
	pipe.Set(ctx, prefix+"at:"+g.Access, rec, s.o.AccessTTL)
	pipe.Set(ctx, prefix+"rt:"+g.Refresh, g.ID, s.o.RefreshTTL)
	pipe.Set(ctx, prefix+"grant:"+g.ID, gb, s.o.RefreshTTL)
	// The person's forge credential outlasts each of their grants.
	pipe.Expire(ctx, prefix+"forge:"+g.person(), s.credTTL())
	pipe.Expire(ctx, prefix+"person-grants:"+g.person(), s.credTTL())
	// A client lasts as long as it's in use.
	pipe.Expire(ctx, prefix+"client:"+g.Client, s.o.RefreshTTL)
	if _, err := pipe.Exec(ctx); err != nil {
		return tokenResponse{}, err
	}
	return tokenResponse{AccessToken: at, TokenType: "Bearer", ExpiresIn: int(s.o.AccessTTL.Seconds()), RefreshToken: rt}, nil
}

// graceRec is kept for RefreshReuseGrace under the hash of a refresh token
// just rotated, so the same client presenting it again gets the same tokens.
// Those are sealed: unlike every other devbot token in Redis, they have to
// be given back.
type graceRec struct {
	Grant  string    `json:"grant"`
	Until  time.Time `json:"until"`
	Tokens string    `json:"tokens"` // sealed graceTokens
}

type graceTokens struct {
	Access  string    `json:"access"`
	Refresh string    `json:"refresh"`
	Expiry  time.Time `json:"exp"`
}

// graceAAD binds sealed grace tokens to their grant and to the refresh
// token they answer, so a record can't be moved to another key.
func graceAAD(gid, refresh string) string { return gid + "\x00rt-grace\x00" + refresh }

// graceGrant is the grant a just-rotated refresh token belongs to while its
// grace window lasts, or "".
func (s *Server) graceGrant(ctx context.Context, refresh string) (string, error) {
	var rec graceRec
	ok, err := s.get(ctx, "rt-grace:"+refresh, &rec)
	if !ok || err != nil || !s.o.Now().Before(rec.Until) {
		return "", err
	}
	return rec.Grant, nil
}

// replay answers a refresh token g has already rotated out, for a caller
// holding g's lock. Within the grace window, and while the tokens it was
// rotated into are still g's current ones, it returns those again. Without
// a grace record it is errGrant, as for any stale token; with one that no
// longer holds, errReused.
func (s *Server) replay(ctx context.Context, g *grant, refresh string) (tokenResponse, error) {
	var rec graceRec
	ok, err := s.get(ctx, "rt-grace:"+refresh, &rec)
	if err != nil {
		return tokenResponse{}, err
	}
	if !ok || !s.o.Now().Before(rec.Until) {
		return tokenResponse{}, errGrant
	}
	b, _, err := s.seal.open(rec.Tokens, graceAAD(g.ID, refresh))
	var t graceTokens
	if err != nil || rec.Grant != g.ID || json.Unmarshal(b, &t) != nil ||
		hash(t.Access) != g.Access || hash(t.Refresh) != g.Refresh || !s.o.Now().Before(t.Expiry) {
		return tokenResponse{}, errReused
	}
	return tokenResponse{AccessToken: t.Access, TokenType: "Bearer", ExpiresIn: int(t.Expiry.Sub(s.o.Now()).Seconds()), RefreshToken: t.Refresh}, nil
}

var (
	errGrant  = errors.New("grant has ended")
	errReused = errors.New("a rotated refresh token was reused")
	errSealed = errors.New("stored forge token can't be opened")
)

// Revoke ends a grant: its tokens stop working, and when it was the person's
// last grant their forge credential is deleted too. It holds the grant's
// lock, so a token request in flight can't write the grant back.
func (s *Server) Revoke(ctx context.Context, gid string) error {
	unlock, err := s.lock(ctx, gid)
	if err != nil {
		return err
	}
	defer unlock()
	return s.endGrant(ctx, gid)
}

// endGrant is Revoke for a caller that holds the lock.
func (s *Server) endGrant(ctx context.Context, gid string) error {
	g, err := s.loadGrant(ctx, gid)
	if err != nil || g == nil {
		return err
	}
	keys := []string{prefix + "grant:" + g.ID}
	if g.Access != "" {
		keys = append(keys, prefix+"at:"+g.Access)
	}
	if g.Refresh != "" {
		keys = append(keys, prefix+"rt:"+g.Refresh)
	}
	if g.Session != "" {
		keys = append(keys, prefix+"session:"+g.Session)
	}
	if g.Retired != "" {
		keys = append(keys, prefix+"rt-grace:"+g.Retired)
	}
	if err := s.o.Redis.Del(ctx, keys...).Err(); err != nil {
		return err
	}
	s.o.Log.Info("auth: grant revoked", "login", g.Login, "client", g.Client)
	s.auditEvent(ctx, "auth.revoke", audit.OK, g.Login, g.Client, map[string]any{"device": g.Device, "ui": g.Session != ""})
	return s.releaseCred(ctx, g.person(), g.ID)
}

// Authenticate resolves a request's bearer token. Anything but a live devbot
// access token for this resource is ErrInvalidToken, a forge token included.
func (s *Server) Authenticate(r *http.Request) (Identity, error) {
	scheme, tok, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || tok == "" {
		return Identity{}, ErrInvalidToken
	}
	ctx := r.Context()
	var rec accessRec
	found, err := s.get(ctx, "at:"+hash(tok), &rec)
	if err != nil {
		return Identity{}, err
	}
	if !found || rec.Audience != s.o.ResourceURL || !s.o.Now().Before(rec.Expiry) {
		return Identity{}, ErrInvalidToken
	}
	g, err := s.loadGrant(ctx, rec.Grant)
	if err != nil {
		return Identity{}, err
	}
	if g == nil || g.Access != hash(tok) || g.Provider != s.o.Forge.Name() {
		return Identity{}, ErrInvalidToken
	}
	return Identity{Grant: g.ID, Provider: g.Provider, Login: g.Login, Client: g.Client}, nil
}

// Challenge is the 401 for ErrInvalidToken, pointing the client at the
// protected-resource metadata so it can discover how to sign in.
func (s *Server) Challenge(w http.ResponseWriter, r *http.Request) {
	v := fmt.Sprintf(`Bearer resource_metadata=%q`, s.o.ResourceURL+"/.well-known/oauth-protected-resource")
	if r.Header.Get("Authorization") != "" {
		v += `, error="invalid_token"`
	}
	w.Header().Set("WWW-Authenticate", v)
	http.Error(w, "unauthorized: sign in to devbot (OAuth)", http.StatusUnauthorized)
}

// forgeToken returns the forge token of the person behind a grant,
// refreshing it first when it is about to expire. Refreshes take the
// person's lock, so the person's grants, on any replica, refresh it once
// between them. A grant whose person has no usable forge credential (the
// forge refused a refresh, or the key it was sealed with is gone) ends.
func (s *Server) forgeToken(ctx context.Context, gid string) (forge.UserToken, error) {
	g, err := s.loadGrant(ctx, gid)
	if err != nil {
		return forge.UserToken{}, err
	}
	if g == nil {
		return forge.UserToken{}, ErrInvalidToken
	}
	p := g.person()
	tok, stale, err := s.readCred(ctx, p)
	if err == nil && !stale && !s.expiring(tok) {
		return tok, nil
	}
	if err != nil && !errors.Is(err, errSealed) && !errors.Is(err, ErrInvalidToken) {
		return forge.UserToken{}, err
	}
	if err == nil {
		tok, err = s.refreshCred(ctx, p)
	}
	if errors.Is(err, errSealed) || errors.Is(err, ErrInvalidToken) {
		s.Revoke(ctx, gid)
		return forge.UserToken{}, ErrInvalidToken
	}
	return tok, err
}

// refreshCred refreshes a person's forge token, or re-seals it under the
// current key, under their lock.
func (s *Server) refreshCred(ctx context.Context, p string) (forge.UserToken, error) {
	unlock, err := s.lock(ctx, personLock(p))
	if err != nil {
		return forge.UserToken{}, err
	}
	defer unlock()
	// Another grant may have refreshed it while this waited for the lock.
	tok, stale, err := s.readCred(ctx, p)
	if errors.Is(err, errSealed) {
		s.dropCred(ctx, p, "")
	}
	if err != nil || (!stale && !s.expiring(tok)) {
		return tok, err
	}
	if s.expiring(tok) {
		fresh, err := s.o.Forge.OAuthRefresh(ctx, tok.Refresh)
		// Only a definite no from the forge ends the person's access; an
		// outage mustn't sign everyone out.
		if errors.Is(err, forge.ErrUnauthorized) {
			s.o.Log.Warn("auth: the forge refused to refresh a person's token; ending their access", "person", p, "err", err)
			s.dropCred(ctx, p, "")
			return forge.UserToken{}, ErrInvalidToken
		}
		if err != nil {
			return forge.UserToken{}, fmt.Errorf("auth: refreshing a forge token: %w", err)
		}
		if fresh.Refresh == "" {
			fresh.Refresh = tok.Refresh
		}
		tok = fresh
	}
	pipe := s.o.Redis.TxPipeline()
	s.writeCred(ctx, pipe, p, tok)
	_, err = pipe.Exec(ctx)
	return tok, err
}

func (s *Server) expiring(t forge.UserToken) bool {
	return !t.Expiry.IsZero() && !s.o.Now().Add(refreshEarly).Before(t.Expiry)
}

// User is the forge as the person behind id, with their own token. If the
// forge rejects the token, the person's forge credential is dropped, the
// grant ends and the call fails with ErrInvalidToken, so the client signs in
// again.
func (s *Server) User(ctx context.Context, id Identity) (forge.User, error) {
	g, err := s.loadGrant(ctx, id.Grant)
	if err != nil {
		return nil, err
	}
	if g == nil {
		return nil, ErrInvalidToken
	}
	tok, err := s.forgeToken(ctx, id.Grant)
	if err != nil {
		return nil, err
	}
	return &user{s: s, grant: id.Grant, person: g.person(), access: tok.Access, u: audit.User(s.o.Forge.AsUser(tok.Access), s.o.Audit, g.Login)}, nil
}

type user struct {
	s              *Server
	grant          string
	person, access string
	u              forge.User
}

func (u *user) check(ctx context.Context, err error) error {
	if errors.Is(err, forge.ErrUnauthorized) {
		u.s.o.Log.Warn("auth: the forge rejected a person's token; ending their access")
		if unlock, lerr := u.s.lock(ctx, personLock(u.person)); lerr == nil {
			u.s.dropCred(ctx, u.person, u.access)
			unlock()
		}
		u.s.Revoke(ctx, u.grant)
		return fmt.Errorf("%w: the forge no longer accepts your sign-in", ErrInvalidToken)
	}
	return err
}

func (u *user) Permission(ctx context.Context, r forge.RepoRef) (forge.Permission, error) {
	p, err := u.u.Permission(ctx, r)
	return p, u.check(ctx, err)
}

func (u *user) CanEditIssues(ctx context.Context, r forge.RepoRef) (bool, error) {
	ok, err := u.u.CanEditIssues(ctx, r)
	return ok, u.check(ctx, err)
}

func (u *user) CreateItem(ctx context.Context, r forge.RepoRef, title, body string, labels []string) (forge.Item, error) {
	it, err := u.u.CreateItem(ctx, r, title, body, labels)
	return it, u.check(ctx, err)
}

func (u *user) CloseItem(ctx context.Context, ref forge.ItemRef) error {
	return u.check(ctx, u.u.CloseItem(ctx, ref))
}

func (u *user) Comment(ctx context.Context, ref forge.ItemRef, body string) error {
	return u.check(ctx, u.u.Comment(ctx, ref, body))
}

func (u *user) SetLabels(ctx context.Context, ref forge.ItemRef, add, remove []string) error {
	return u.check(ctx, u.u.SetLabels(ctx, ref, add, remove))
}
