package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"git.shdw.tech/rob/devbot/internal/forge"
	"git.shdw.tech/rob/devbot/internal/forge/fake"
)

const (
	oldKey = "the-old-auth-key-0123456789abcdef"
	newKey = "the-new-auth-key-0123456789abcdef"
)

func TestSealRoundTripAndBinding(t *testing.T) {
	s, err := newSealer([]byte(newKey), nil)
	if err != nil {
		t.Fatal(err)
	}
	sealed := s.seal([]byte("forge-token"), "grant-1")
	if strings.Contains(sealed, "forge-token") {
		t.Fatal("sealed value contains the plaintext")
	}
	if got, stale, err := s.open(sealed, "grant-1"); err != nil || stale || string(got) != "forge-token" {
		t.Fatalf("open: %q %v %v", got, stale, err)
	}
	if _, _, err := s.open(sealed, "grant-2"); err == nil {
		t.Fatal("a sealed token moved to another grant must not open")
	}
	if _, err := newSealer([]byte("short"), nil); err == nil {
		t.Fatal("a short key must be refused")
	}
}

// Rotation: the new key seals, the previous one still opens until
// everything has been re-sealed, and without it old values don't open.
func TestSealRotation(t *testing.T) {
	old, _ := newSealer([]byte(oldKey), nil)
	sealed := old.seal([]byte("forge-token"), "g")
	rotating, err := newSealer([]byte(newKey), []byte(oldKey))
	if err != nil {
		t.Fatal(err)
	}
	got, stale, err := rotating.open(sealed, "g")
	if err != nil || !stale || string(got) != "forge-token" {
		t.Fatalf("the previous key should open, flagged for re-sealing: %q %v %v", got, stale, err)
	}
	done, _ := newSealer([]byte(newKey), nil)
	if _, _, err := done.open(sealed, "g"); err == nil {
		t.Fatal("without the previous key, an old value must not open")
	}
}

// Using a grant during a rotation re-seals its forge token under the new key.
func TestForgeTokenResealedOnRotation(t *testing.T) {
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	f := fake.New()
	opts := Options{ResourceURL: "https://devbot.example", Forge: f, Redis: rc, Key: []byte(oldKey), AccessTTL: time.Hour, RefreshTTL: time.Hour}
	before, _ := New(opts)
	ctx := context.Background()
	g, err := before.newGrant(ctx, "client", "alice", f.TokenFor("alice"), false)
	if err != nil {
		t.Fatal(err)
	}
	opts.Key, opts.PreviousKey = []byte(newKey), []byte(oldKey)
	during, _ := New(opts)
	if _, err := during.User(ctx, Identity{Grant: g.ID}); err != nil {
		t.Fatal(err)
	}
	opts.PreviousKey = nil
	after, _ := New(opts)
	if _, err := after.User(ctx, Identity{Grant: g.ID}); err != nil {
		t.Fatalf("the grant should have been re-sealed under the new key: %v", err)
	}
	// A grant nobody used during the rotation has to sign in again.
	stale, _ := before.newGrant(ctx, "client", "bob", f.TokenFor("bob"), false)
	if _, err := after.User(ctx, Identity{Grant: stale.ID}); err != ErrInvalidToken {
		t.Fatalf("want ErrInvalidToken, got %v", err)
	}
	b, _ := rc.Get(ctx, prefix+"forge:fake:alice").Bytes()
	if len(b) == 0 || strings.Contains(string(b), "fake-") {
		t.Fatal("a forge token is stored in the clear")
	}
}

// Someone who can write to Redis but hasn't the auth key can't make a token:
// grant and access-token records written straight in, edited, or moved to
// another key don't authenticate.
func TestForgedRecordsDontAuthenticate(t *testing.T) {
	v := newSharedEnv(t)
	ctx := context.Background()
	g, err := v.s.newGrant(ctx, "devbot_c_x", "alice", v.f.TokenFor("alice"), false)
	if err != nil {
		t.Fatal(err)
	}
	real, err := v.s.issue(ctx, g.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	authn := func(tok string) error {
		_, err := v.s.Authenticate(newRequest("Bearer " + tok))
		return err
	}
	if err := authn(real.AccessToken); err != nil {
		t.Fatalf("the real token: %v", err)
	}

	mine := "devbot_at_chosen-by-whoever-writes-to-redis"
	exp := v.s.o.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	forgedGrant := `{"id":"gforged","client":"devbot_c_x","provider":"fake","login":"alice","access":"` + hash(mine) + `","created":"2026-01-01T00:00:00Z"}`
	forgedAccess := `{"grant":"gforged","aud":"https://devbot.example","exp":"` + exp + `"}`
	for name, mac := range map[string]string{"no MAC": "", "a made-up MAC": "m1.AAAA."} {
		v.rc.Set(ctx, prefix+"grant:gforged", mac+forgedGrant, time.Hour)
		v.rc.Set(ctx, prefix+"at:"+hash(mine), mac+forgedAccess, time.Hour)
		if err := authn(mine); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("a forged grant and token with %s: want ErrInvalidToken, got %v", name, err)
		}
	}

	// alice's real grant, pointed at a token of the writer's choosing.
	b, _ := v.rc.Get(ctx, prefix+"grant:"+g.ID).Bytes()
	edited := strings.Replace(string(b), hash(real.AccessToken), hash(mine), 1)
	if edited == string(b) {
		t.Fatal("the grant doesn't name its access token's hash")
	}
	v.rc.Set(ctx, prefix+"grant:"+g.ID, edited, time.Hour)
	at, _ := v.rc.Get(ctx, prefix+"at:"+hash(real.AccessToken)).Bytes()
	v.rc.Set(ctx, prefix+"at:"+hash(mine), at, time.Hour)
	if err := authn(mine); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("an edited grant and a moved token record: want ErrInvalidToken, got %v", err)
	}
	if err := authn(real.AccessToken); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("an edited grant must not open for its real token either: %v", err)
	}
}

// Records are MAC'd under the current key, and verify under the previous one
// during a rotation, flagged for signing again.
func TestRecordMACRotation(t *testing.T) {
	old, _ := newSealer([]byte(oldKey), nil)
	signed := old.sign("devbot:auth:grant:g", []byte(`{"login":"alice"}`))
	if rec, stale, ok := old.verify("devbot:auth:grant:g", signed); !ok || stale || string(rec) != `{"login":"alice"}` {
		t.Fatalf("verify: %q %v %v", rec, stale, ok)
	}
	if _, _, ok := old.verify("devbot:auth:grant:h", signed); ok {
		t.Fatal("a record moved to another key must not verify")
	}
	rotating, _ := newSealer([]byte(newKey), []byte(oldKey))
	if _, stale, ok := rotating.verify("devbot:auth:grant:g", signed); !ok || !stale {
		t.Fatalf("the previous key should verify, flagged stale: %v %v", stale, ok)
	}
	done, _ := newSealer([]byte(newKey), nil)
	if _, _, ok := done.verify("devbot:auth:grant:g", signed); ok {
		t.Fatal("without the previous key, an old record must not verify")
	}
}
func TestRedirectRules(t *testing.T) {
	for uri, want := range map[string]bool{
		"https://claude.ai/api/mcp/auth_callback": true,
		"http://127.0.0.1:33418/callback":         true,
		"http://localhost/cb":                     true,
		"cursor://anysphere.cursor-retrieval/cb":  true,
		"http://app.example/cb":                   false,
		"javascript:alert(1)":                     false,
		"https://app.example/cb#frag":             false,
		"/relative":                               false,
	} {
		if got := redirectURIAllowed(uri); got != want {
			t.Errorf("redirectURIAllowed(%q) = %v", uri, got)
		}
	}
	reg := []string{"http://127.0.0.1/callback", "https://app.example/cb"}
	for uri, want := range map[string]bool{
		"http://127.0.0.1:5555/callback": true, // any port on loopback
		"http://127.0.0.1:5555/other":    false,
		"https://app.example/cb":         true,
		"https://app.example:8443/cb":    false, // ports matter off loopback
		"https://app.example/cb?x=1":     false,
	} {
		if got := redirectMatch(reg, uri); got != want {
			t.Errorf("redirectMatch(%q) = %v", uri, got)
		}
	}
}

func TestResourceIndicator(t *testing.T) {
	s := &Server{o: Options{ResourceURL: "https://devbot.example"}}
	for res, want := range map[string]bool{
		"": true, "https://devbot.example": true, "https://devbot.example/": true, "https://devbot.example/mcp": true,
		"https://devbot.example.evil": false, "https://other.example/mcp": false, "https://devbot.example/mcp#x": false,
	} {
		if got := s.resourceOK(res); got != want {
			t.Errorf("resourceOK(%q) = %v", res, got)
		}
	}
}

func TestUserCodes(t *testing.T) {
	c := newUserCode()
	if len(c) != 9 || c[4] != '-' || normalizeUserCode(strings.ToLower(strings.ReplaceAll(c, "-", " "))) != c {
		t.Fatalf("user code %q doesn't normalise back", c)
	}
	if normalizeUserCode("ABC") != "" {
		t.Fatal("a short code must not normalise")
	}
}

// The fake forge's token can't authenticate to devbot: there is no
// passthrough.
func TestForgeTokenIsNotADevbotToken(t *testing.T) {
	mr := miniredis.RunT(t)
	f := fake.New()
	s, _ := New(Options{ResourceURL: "https://devbot.example", Forge: f, Redis: redis.NewClient(&redis.Options{Addr: mr.Addr()}),
		Key: []byte(newKey), AccessTTL: time.Hour, RefreshTTL: time.Hour})
	r := newRequest("Bearer " + f.TokenFor("alice").Access)
	if _, err := s.Authenticate(r); err != ErrInvalidToken {
		t.Fatalf("want ErrInvalidToken, got %v", err)
	}
}

func newRequest(authz string) *http.Request {
	r := httptest.NewRequest("POST", "/mcp", nil)
	r.Header.Set("Authorization", authz)
	return r
}

// Behind a proxy that strips a path prefix, the browser still sends the
// prefix, so the cookies are scoped under it.
func TestCookiesUnderPathPrefix(t *testing.T) {
	mr := miniredis.RunT(t)
	s, err := New(Options{ResourceURL: "https://devbot.example.com/devbot/", Forge: fake.New(), Redis: redis.NewClient(&redis.Options{Addr: mr.Addr()}), Key: []byte(newKey)})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.SignIn(w, httptest.NewRequest("GET", "/ui/login", nil))
	cs := w.Result().Cookies()
	if w.Code != 302 || len(cs) != 1 || cs[0].Path != "/devbot/oauth/" || !cs[0].Secure || !cs[0].HttpOnly {
		t.Fatalf("sign-in cookie: %d %+v", w.Code, cs)
	}
	if !strings.Contains(w.Header().Get("Location"), "devbot.example.com%2Fdevbot%2Foauth%2Fcallback") {
		t.Fatalf("the forge should come back under the prefix: %s", w.Header().Get("Location"))
	}
	if p := s.cookiePath("/ui/"); p != "/devbot/ui/" {
		t.Fatalf("session cookie path %q", p)
	}
}

// sharedEnv is a broker on a clock the test moves, in front of a fake forge
// with Gitea's one OAuth grant per user and app, whose tokens last 10
// minutes.
type sharedEnv struct {
	t  *testing.T
	s  *Server
	f  *fake.Forge
	rc *redis.Client

	mu  sync.Mutex
	now time.Time
}

func newSharedEnv(t *testing.T) *sharedEnv {
	v := &sharedEnv{t: t, f: fake.New(), now: time.Now()}
	v.f.OneGrantPerUser(true)
	v.f.SetTokenTTL(10 * time.Minute)
	v.rc = redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	s, err := New(Options{ResourceURL: "https://devbot.example", Forge: v.f, Redis: v.rc, Key: []byte(newKey),
		AccessTTL: time.Hour, RefreshTTL: 30 * 24 * time.Hour, Now: func() time.Time {
			v.mu.Lock()
			defer v.mu.Unlock()
			return v.now
		}})
	if err != nil {
		t.Fatal(err)
	}
	v.s = s
	v.f.SetClock(s.o.Now)
	return v
}

// advance moves past most of a forge token's life, into the refresh margin.
func (v *sharedEnv) advance() {
	v.mu.Lock()
	v.now = v.now.Add(9*time.Minute + 30*time.Second)
	v.mu.Unlock()
}

// signIn is a sign-in from client: a new code exchange at the forge, and a
// devbot grant with tokens issued.
func (v *sharedEnv) signIn(client, login string) string {
	v.t.Helper()
	ctx := context.Background()
	g, err := v.s.newGrant(ctx, client, login, v.f.TokenFor(login), false)
	if err == nil {
		_, err = v.s.issue(ctx, g.ID, "")
	}
	if err != nil {
		v.t.Fatal(err)
	}
	return g.ID
}

// works checks the grant's forge token is one the forge accepts.
func (v *sharedEnv) works(gid string) {
	v.t.Helper()
	ctx := context.Background()
	tok, err := v.s.forgeToken(ctx, gid)
	if err != nil {
		v.t.Fatalf("grant %s: %v", gid, err)
	}
	if _, err := v.f.CurrentUser(ctx, tok.Access); err != nil {
		v.t.Fatalf("grant %s: the forge rejects its token: %v", gid, err)
	}
}

func (v *sharedEnv) hasCred(login string) bool {
	n, _ := v.rc.Exists(context.Background(), prefix+"forge:fake:"+login).Result()
	return n == 1
}

// Three sign-ins for one person, refreshing at different times, all keep
// working against a forge where each refresh invalidates every older
// refresh token. Each new sign-in replaces the forge token, without breaking
// the older ones.
func TestSharedForgeCredentialAcrossGrants(t *testing.T) {
	v := newSharedEnv(t)
	ui := v.signIn("devbot-ui", "alice")
	v.works(ui)
	code := v.signIn("devbot_c_code", "alice")
	v.works(ui)
	v.advance()
	v.works(ui) // refreshes with the token code's sign-in got
	device := v.signIn("devbot_c_device", "alice")
	v.advance()
	v.works(code)
	v.works(ui)
	v.advance()
	v.works(device)
	v.works(code)
	v.works(ui)
	if n := v.f.Refreshes(); n != 3 {
		t.Fatalf("want one forge refresh per expiry, got %d", n)
	}
	b, _ := v.rc.Get(context.Background(), prefix+"grant:"+ui).Bytes()
	if strings.Contains(string(b), "fake-") || strings.Contains(string(b), "v1.") {
		t.Fatalf("a grant carries a forge token of its own: %s", b)
	}
}

// Every grant of one person asking for an expiring forge token at once: one
// forge refresh between them, and all get its result.
func TestConcurrentForgeRefreshOnce(t *testing.T) {
	v := newSharedEnv(t)
	var gids []string
	for _, c := range []string{"devbot-ui", "devbot_c_a", "devbot_c_b", "devbot_c_c"} {
		gids = append(gids, v.signIn(c, "alice"))
	}
	v.advance()
	var wg sync.WaitGroup
	toks := make([]forge.UserToken, 8)
	errs := make([]error, 8)
	for i := range toks {
		wg.Go(func() { toks[i], errs[i] = v.s.forgeToken(context.Background(), gids[i%len(gids)]) })
	}
	wg.Wait()
	for i := range toks {
		if errs[i] != nil || toks[i].Access != toks[0].Access {
			t.Fatalf("caller %d: %v, or a token other than the first caller's", i, errs[i])
		}
	}
	if n := v.f.Refreshes(); n != 1 {
		t.Fatalf("want one forge refresh, got %d", n)
	}
	for _, g := range gids {
		v.works(g)
	}
}

// Revoking one grant leaves the person's others working; revoking the last
// deletes the forge credential.
func TestRevokeReleasesSharedCredential(t *testing.T) {
	v := newSharedEnv(t)
	ctx := context.Background()
	a, b := v.signIn("devbot-ui", "alice"), v.signIn("devbot_c_code", "alice")
	if err := v.s.Revoke(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := v.s.forgeToken(ctx, a); err != ErrInvalidToken {
		t.Fatalf("a revoked grant: want ErrInvalidToken, got %v", err)
	}
	v.advance()
	v.works(b)
	if !v.hasCred("alice") {
		t.Fatal("the credential should stay while a grant uses it")
	}
	// Run twice: the second has nothing left to do.
	for range 2 {
		if err := v.s.Revoke(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	if v.hasCred("alice") {
		t.Fatal("the last grant's revocation should delete the forge credential")
	}
	// A grant that expired on its own doesn't keep the credential either.
	c, d := v.signIn("devbot-ui", "alice"), v.signIn("devbot_c_code", "alice")
	v.rc.Del(ctx, prefix+"grant:"+c)
	v.s.Revoke(ctx, d)
	if v.hasCred("alice") {
		t.Fatal("an expired grant shouldn't keep the forge credential")
	}
}

// A forge that refuses the refresh ends the person's access through every
// grant, and nobody else's.
func TestForgeRefusalEndsPersonsGrants(t *testing.T) {
	v := newSharedEnv(t)
	ctx := context.Background()
	a, b := v.signIn("devbot-ui", "alice"), v.signIn("devbot_c_code", "alice")
	bob := v.signIn("devbot-ui", "bob")
	v.advance()
	v.f.FailRefresh(true)
	if _, err := v.s.forgeToken(ctx, a); err != ErrInvalidToken {
		t.Fatalf("want ErrInvalidToken, got %v", err)
	}
	v.f.FailRefresh(false)
	if _, err := v.s.forgeToken(ctx, b); err != ErrInvalidToken {
		t.Fatalf("the person's other grant: want ErrInvalidToken, got %v", err)
	}
	for _, g := range []string{a, b} {
		if n, _ := v.rc.Exists(ctx, prefix+"grant:"+g).Result(); n != 0 {
			t.Fatalf("grant %s should have ended", g)
		}
	}
	if v.hasCred("alice") {
		t.Fatal("the refused credential should be gone")
	}
	v.works(bob)
}

// The forge rejecting the token a call was made with drops the person's
// credential, unless it has already been replaced.
func TestForgeRejectionDropsCredential(t *testing.T) {
	v := newSharedEnv(t)
	ctx := context.Background()
	a, b := v.signIn("devbot-ui", "alice"), v.signIn("devbot_c_code", "alice")
	u, err := v.s.User(ctx, Identity{Grant: a})
	if err != nil {
		t.Fatal(err)
	}
	v.f.RefuseUserWrites(true)
	if err := u.(*user).check(ctx, forge.ErrUnauthorized); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("want ErrInvalidToken, got %v", err)
	}
	if _, err := v.s.forgeToken(ctx, b); err != ErrInvalidToken {
		t.Fatalf("the person's other grant: want ErrInvalidToken, got %v", err)
	}

	c := v.signIn("devbot-ui", "alice")
	u, _ = v.s.User(ctx, Identity{Grant: c})
	d := v.signIn("devbot_c_code", "alice") // replaces the token u holds
	u.(*user).check(ctx, forge.ErrUnauthorized)
	v.works(d)
}

// A forge that rotates refresh tokens (GitHub, Gitea) refuses a used one,
// so two refreshes of one person's forge token at once would sign them
// out. The person's lock serializes them, across processes sharing Redis
// too: one refresh, and every caller gets the fresh token.
func TestForgeTokenRefreshIsSerialized(t *testing.T) {
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	f := fake.New()
	opts := Options{ResourceURL: "https://devbot.example", Forge: f, Redis: rc, Key: []byte(newKey), AccessTTL: time.Hour, RefreshTTL: time.Hour}
	replicas := make([]*Server, 2)
	for i := range replicas {
		replicas[i], _ = New(opts)
	}
	ctx := context.Background()
	f.SetTokenTTL(30 * time.Second) // inside the refresh margin: due now
	g, err := replicas[0].newGrant(ctx, "client", "alice", f.TokenFor("alice"), false)
	if err != nil {
		t.Fatal(err)
	}
	f.SetTokenTTL(time.Hour)
	before := f.Refreshes()
	start := make(chan struct{})
	tokens := make(chan string, 20)
	var wg sync.WaitGroup
	for i := range cap(tokens) {
		wg.Go(func() {
			<-start
			tok, err := replicas[i%2].forgeToken(ctx, g.ID)
			if err != nil {
				t.Errorf("caller %d: %v", i, err)
			}
			tokens <- tok.Access
		})
	}
	close(start)
	wg.Wait()
	close(tokens)
	if n := f.Refreshes() - before; n != 1 {
		t.Fatalf("%d refreshes, want 1", n)
	}
	var first string
	for tok := range tokens {
		if first == "" {
			first = tok
		}
		if tok != first {
			t.Fatal("callers got different tokens")
		}
	}
	if login, err := f.CurrentUser(ctx, first); login != "alice" || err != nil {
		t.Fatalf("the refreshed token: %q, %v", login, err)
	}
	if _, err := replicas[1].User(ctx, Identity{Grant: g.ID}); err != nil {
		t.Fatalf("still signed in: %v", err)
	}
}

// failSessions is Redis refusing to write UI sessions, and only those.
type failSessions struct{}

func (failSessions) DialHook(next redis.DialHook) redis.DialHook { return next }
func (failSessions) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (failSessions) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "set" && strings.Contains(fmt.Sprint(cmd.Args()[1]), "session:") {
			err := errors.New("OOM command not allowed when used memory > 'maxmemory'")
			cmd.SetErr(err)
			return err
		}
		return next(ctx, cmd)
	}
}

// A sign-in Redis didn't record is a failure, not a cookie for a session
// that doesn't exist.
func TestSessionWriteFailureIsAnError(t *testing.T) {
	rc := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	rc.AddHook(failSessions{})
	f := fake.New()
	s, err := New(Options{ResourceURL: "https://devbot.example", Forge: f, Redis: rc, Key: []byte(newKey), AccessTTL: time.Hour, RefreshTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.startSession(w, httptest.NewRequest("GET", "/oauth/callback", nil), "alice", f.TokenFor("alice"))
	if w.Code != 500 || w.Header().Get("Set-Cookie") != "" {
		t.Fatalf("a failed session write: %d, Set-Cookie %q", w.Code, w.Header().Get("Set-Cookie"))
	}
}
