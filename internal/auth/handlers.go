package auth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/oauth2"

	"git.shdw.tech/rob/devbot/internal/audit"
)

const (
	grantCode    = "authorization_code"
	grantRefresh = "refresh_token"
	grantDevice  = "urn:ietf:params:oauth:grant-type:device_code"
)

// Routes adds the OAuth endpoints and the discovery documents to mux.
func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", s.protectedResource)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/{path...}", s.protectedResource)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", s.serverMetadata)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server/{path...}", s.serverMetadata)
	l := s.o.Limits
	mux.HandleFunc("POST /oauth/register", s.limited("register", l.Register, false, "temporarily_unavailable", s.register))
	mux.HandleFunc("GET /oauth/authorize", s.limited("authorize", l.Authorize, true, "", s.authorize))
	mux.HandleFunc("GET /oauth/callback", s.callback)
	mux.HandleFunc("POST /oauth/consent", s.consent)
	// slow_down, so a polling device backs off instead of giving up.
	mux.HandleFunc("POST /oauth/token", s.limited("token", l.Token, false, "slow_down", s.token))
	mux.HandleFunc("POST /oauth/revoke", s.revoke)
	mux.HandleFunc("POST /oauth/device_authorization", s.limited("device_authorization", l.DeviceAuthorization, false, "temporarily_unavailable", s.deviceAuthorization))
	mux.HandleFunc("GET /oauth/device", s.limited("device", l.Device, true, "", s.devicePage))
	mux.HandleFunc("POST /oauth/device", s.limited("device", l.Device, true, "", s.deviceSubmit))
}

func (s *Server) url(path string) string { return s.o.ResourceURL + path }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// oauthError is RFC 6749's error response.
func oauthError(w http.ResponseWriter, code int, errCode, desc string) {
	if code == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Basic realm="devbot"`)
	}
	writeJSON(w, code, map[string]string{"error": errCode, "error_description": desc})
}

// ---- discovery ----

// protectedResource is RFC 9728 metadata. Asked for under a path (the MCP
// endpoint's /mcp), the resource is that URL, as RFC 9728 requires; tokens
// for the base URL cover everything under it.
func (s *Server) protectedResource(w http.ResponseWriter, r *http.Request) {
	res := s.o.ResourceURL
	if p := r.PathValue("path"); p != "" {
		res += "/" + p
	}
	writeJSON(w, 200, map[string]any{
		"resource":                 res,
		"authorization_servers":    []string{s.o.ResourceURL},
		"bearer_methods_supported": []string{"header"},
		"resource_name":            "devbot",
	})
}

// serverMetadata is RFC 8414 metadata.
func (s *Server) serverMetadata(w http.ResponseWriter, r *http.Request) {
	auth := []string{"none", "client_secret_basic", "client_secret_post"}
	m := map[string]any{
		"issuer":                                         s.o.ResourceURL,
		"authorization_endpoint":                         s.url("/oauth/authorize"),
		"token_endpoint":                                 s.url("/oauth/token"),
		"registration_endpoint":                          s.url("/oauth/register"),
		"revocation_endpoint":                            s.url("/oauth/revoke"),
		"response_types_supported":                       []string{"code"},
		"grant_types_supported":                          []string{grantCode, grantRefresh},
		"code_challenge_methods_supported":               []string{"S256"},
		"token_endpoint_auth_methods_supported":          auth,
		"revocation_endpoint_auth_methods_supported":     auth,
		"authorization_response_iss_parameter_supported": true,
	}
	if s.deviceGrant() {
		m["device_authorization_endpoint"] = s.url("/oauth/device_authorization")
		m["grant_types_supported"] = []string{grantCode, grantRefresh, grantDevice}
	}
	writeJSON(w, 200, m)
}

// deviceGrant reports whether clients may use the device grant. Every
// client registers itself (RFC 7591), so the install's switch for
// dynamically registered clients is the switch for all of them.
func (s *Server) deviceGrant() bool { return s.o.DeviceGrant }

// resourceOK checks an RFC 8707 resource indicator: devbot's own URL or
// one under it. Every token devbot issues is bound to its base URL.
func (s *Server) resourceOK(res string) bool {
	if res == "" {
		return true
	}
	u, err := url.Parse(res)
	if err != nil || u.Fragment != "" {
		return false
	}
	res = strings.TrimRight(res, "/")
	return res == s.o.ResourceURL || strings.HasPrefix(res, s.o.ResourceURL+"/")
}

// ---- dynamic client registration (RFC 7591) ----

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	fail := func(code int, errCode, desc string) {
		s.auditEvent(r.Context(), "auth.client.register", failure(code), "", "", map[string]any{"reason": errCode})
		oauthError(w, code, errCode, desc)
	}
	var req struct {
		RedirectURIs []string `json:"redirect_uris"`
		AuthMethod   string   `json:"token_endpoint_auth_method"`
		GrantTypes   []string `json:"grant_types"`
		Name         string   `json:"client_name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		fail(400, "invalid_client_metadata", "body must be a JSON client metadata document")
		return
	}
	if req.AuthMethod == "" {
		req.AuthMethod = "client_secret_basic" // RFC 7591's default
	}
	if !slices.Contains([]string{"none", "client_secret_basic", "client_secret_post"}, req.AuthMethod) {
		fail(400, "invalid_client_metadata", "token_endpoint_auth_method must be none, client_secret_basic or client_secret_post")
		return
	}
	if len(req.GrantTypes) == 0 {
		req.GrantTypes = []string{grantCode}
	}
	for _, g := range req.GrantTypes {
		if g != grantCode && g != grantRefresh && (g != grantDevice || !s.deviceGrant()) {
			fail(400, "invalid_client_metadata", "unsupported grant type "+g)
			return
		}
	}
	if slices.Contains(req.GrantTypes, grantCode) && len(req.RedirectURIs) == 0 {
		fail(400, "invalid_redirect_uri", "redirect_uris is required for the authorization_code grant")
		return
	}
	if len(req.RedirectURIs) > 10 || len(req.Name) > 200 {
		fail(400, "invalid_client_metadata", "too many redirect_uris, or client_name too long")
		return
	}
	for _, u := range req.RedirectURIs {
		if !redirectURIAllowed(u) {
			fail(400, "invalid_redirect_uri", "redirect URI "+u+" must be https, a loopback http URL, or an app's own scheme")
			return
		}
	}
	c := client{ID: randomToken("devbot_c_"), Name: req.Name, RedirectURIs: req.RedirectURIs, GrantTypes: req.GrantTypes,
		AuthMethod: req.AuthMethod, IssuedAt: s.o.Now().Unix()}
	var secret string
	if c.AuthMethod != "none" {
		secret = randomToken("devbot_cs_")
		c.SecretHash = hash(secret)
	}
	// Registration is open to anyone, so one address gets only so many a
	// day, and one nobody signs in with lapses in a day; issuing tokens
	// extends it to refresh_token_ttl.
	if ip := s.clientIP(r); ip != "" && s.o.Limits.ClientsPerAddress > 0 &&
		s.count(r.Context(), "clients:"+addrKey(ip), clientTTL) > int64(s.o.Limits.ClientsPerAddress) {
		w.Header().Set("Retry-After", strconv.Itoa(int(clientTTL.Seconds())))
		oauthError(w, http.StatusTooManyRequests, "temporarily_unavailable", "this address has registered too many clients today")
		return
	}
	if err := s.set(r.Context(), "client:"+c.ID, c, clientTTL); err != nil {
		fail(500, "server_error", "couldn't store the client")
		return
	}
	resp := map[string]any{
		"client_id": c.ID, "client_id_issued_at": c.IssuedAt, "redirect_uris": c.RedirectURIs,
		"grant_types": c.GrantTypes, "token_endpoint_auth_method": c.AuthMethod, "response_types": []string{"code"},
	}
	if c.Name != "" {
		resp["client_name"] = c.Name
	}
	if secret != "" {
		resp["client_secret"], resp["client_secret_expires_at"] = secret, 0
	}
	s.auditEvent(r.Context(), "auth.client.register", audit.OK, "", c.ID, map[string]any{"client_name": c.Name, "grant_types": c.GrantTypes,
		"auth_method": c.AuthMethod, "redirect_uris": c.RedirectURIs})
	writeJSON(w, http.StatusCreated, resp)
}

// redirectURIAllowed follows RFC 8252: https, http only to a loopback
// address, or a native app's private scheme. Never a scheme a browser would
// run or read locally.
func redirectURIAllowed(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Fragment != "" {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return u.Host != ""
	case "http":
		return isLoopback(u.Hostname())
	case "javascript", "data", "file", "vbscript", "about", "blob", "ftp", "ws", "wss", "intent":
		return false
	}
	return true
}

func isLoopback(host string) bool { return host == "localhost" || host == "127.0.0.1" || host == "::1" }

// redirectMatch is exact matching, except that a loopback redirect may use
// any port (RFC 8252 7.3): native apps listen on whatever port is free.
func redirectMatch(registered []string, got string) bool {
	if slices.Contains(registered, got) {
		return true
	}
	g, err := url.Parse(got)
	if err != nil || g.Fragment != "" || g.Scheme != "http" || !isLoopback(g.Hostname()) {
		return false
	}
	for _, r := range registered {
		ru, err := url.Parse(r)
		if err == nil && ru.Scheme == "http" && ru.Hostname() == g.Hostname() && ru.Path == g.Path && ru.RawQuery == g.RawQuery {
			return true
		}
	}
	return false
}

// ---- authorization code flow ----

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ctx := r.Context()
	c, err := s.loadClient(ctx, q.Get("client_id"))
	if err != nil {
		page(w, 500, "Something went wrong", "devbot couldn't read its client registry. Try again.")
		return
	}
	// Until the client and its redirect check out, errors can't be sent
	// back to the client: that would be an open redirect.
	if c == nil {
		page(w, 400, "Unknown client", "This app isn't registered with devbot, or its registration lapsed. Reconnect it so it registers again.")
		return
	}
	redirect := q.Get("redirect_uri")
	if redirect == "" && len(c.RedirectURIs) == 1 {
		redirect = c.RedirectURIs[0]
	}
	if redirect == "" || !redirectMatch(c.RedirectURIs, redirect) {
		page(w, 400, "Redirect not registered", "The app asked devbot to send you back to an address it didn't register.")
		return
	}
	fail := func(code, desc string) {
		s.auditEvent(ctx, "auth.sign_in", audit.Refused, "", c.ID, map[string]any{"reason": code})
		v := url.Values{"error": {code}, "error_description": {desc}, "iss": {s.o.ResourceURL}}
		if st := q.Get("state"); st != "" {
			v.Set("state", st)
		}
		http.Redirect(w, r, withQuery(redirect, v), http.StatusFound)
	}
	switch {
	case !c.allows(grantCode):
		fail("unauthorized_client", "this client isn't registered for the authorization_code grant")
		return
	case q.Get("response_type") != "code":
		fail("unsupported_response_type", "only response_type=code is supported")
		return
	case q.Get("code_challenge_method") != "S256" || !validChallenge(q.Get("code_challenge")):
		fail("invalid_request", "PKCE is required: send a code_challenge with code_challenge_method=S256")
		return
	case !s.resourceOK(q.Get("resource")):
		fail("invalid_target", "this server's resource is "+s.o.ResourceURL)
		return
	}
	p := pending{Client: c.ID, RedirectURI: q.Get("redirect_uri"), Redirect: redirect, State: q.Get("state"),
		Challenge: q.Get("code_challenge"), Verifier: oauth2.GenerateVerifier()}
	s.toForge(w, r, p)
}

func validChallenge(c string) bool {
	if len(c) < 43 || len(c) > 128 {
		return false
	}
	for _, r := range c {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || strings.ContainsRune("-._~", r)) {
			return false
		}
	}
	return true
}

func withQuery(base string, v url.Values) string {
	if strings.Contains(base, "?") {
		return base + "&" + v.Encode()
	}
	return base + "?" + v.Encode()
}

// signinCookie binds a sign-in to the browser that started it, so a forge
// login link someone else started, forwarded to a victim, can't be completed
// in the victim's browser. It does nothing against device-code phishing,
// where the victim is sent the code and starts the sign-in themselves: that
// is what the device page's and consent page's warnings, the lack of a
// pre-filled verification link, and auth.dynamic_device_grant are for.
const signinCookie = "devbot_signin"

// toForge parks the request and sends the person to the forge's login and
// consent page for devbot's app, with devbot's own PKCE.
func (s *Server) toForge(w http.ResponseWriter, r *http.Request, p pending) {
	state, nonce := randomToken(""), randomToken("")
	p.Browser = hash(nonce)
	if err := s.set(r.Context(), "pending:"+state, p, pendingTTL); err != nil {
		page(w, 500, "Something went wrong", "devbot couldn't start the sign-in. Try again.")
		return
	}
	u, err := s.o.Forge.OAuthURL(s.url("/oauth/callback"), state, oauth2.S256ChallengeFromVerifier(p.Verifier))
	if err != nil {
		s.o.Log.Error("auth: no forge OAuth app", "err", err)
		page(w, 500, "Sign-in isn't set up", "This devbot install has no OAuth app on its forge.")
		return
	}
	// Lax still sends it on the forge's top-level redirect back.
	http.SetCookie(w, &http.Cookie{Name: signinCookie, Value: nonce, Path: s.cookiePath("/oauth/"), MaxAge: int(pendingTTL.Seconds()),
		HttpOnly: true, Secure: strings.HasPrefix(s.o.ResourceURL, "https://"), SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, u, http.StatusFound)
}

func sameBrowser(r *http.Request, want string) bool {
	c, err := r.Cookie(signinCookie)
	return err == nil && want != "" && subtle.ConstantTimeCompare([]byte(hash(c.Value)), []byte(want)) == 1
}

// consentRec is a forge sign-in waiting for the person to approve the client.
type consentRec struct {
	P     pending `json:"p"`
	Grant string  `json:"grant"`
	Login string  `json:"login"`
}

// back returns the authorization result to the client's redirect URI.
func (s *Server) back(w http.ResponseWriter, r *http.Request, p pending, v url.Values) {
	v.Set("iss", s.o.ResourceURL)
	if p.State != "" {
		v.Set("state", p.State)
	}
	http.Redirect(w, r, withQuery(p.Redirect, v), http.StatusFound)
}

// callback is where the forge sends the person back. devbot exchanges the
// forge's code for their forge token, learns who they are, records a grant,
// and asks them whether the client may act as them.
func (s *Server) callback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	var p pending
	ok, err := s.getDel(ctx, "pending:"+q.Get("state"), &p)
	if err != nil || !ok {
		page(w, 400, "Sign-in expired", "This sign-in link was already used or has expired. Start again from your app.")
		return
	}
	if !sameBrowser(r, p.Browser) {
		page(w, 400, "Sign-in started elsewhere", "This sign-in was started in another browser, so devbot won't finish it here. Start again from your app or device.")
		return
	}
	if e := q.Get("error"); e != "" {
		s.auditEvent(ctx, "auth.sign_in", audit.Denied, "", p.Client, map[string]any{"reason": "the forge did not approve", "ui": p.UI, "device": p.Device != ""})
		if p.UI {
			page(w, 403, "Sign-in cancelled", "The forge didn't approve the sign-in.")
			return
		}
		if p.Device != "" {
			s.updateDevice(r, p.Device, func(d *deviceRec) { d.Denied = true })
			page(w, 403, "Sign-in cancelled", "The forge didn't approve the sign-in, so the device wasn't signed in.")
			return
		}
		s.back(w, r, p, url.Values{"error": {"access_denied"}, "error_description": {"the forge sign-in was not approved"}})
		return
	}
	tok, err := s.o.Forge.OAuthExchange(ctx, s.url("/oauth/callback"), q.Get("code"), p.Verifier)
	var login string
	if err == nil {
		login, err = s.o.Forge.CurrentUser(ctx, tok.Access)
	}
	if err != nil {
		s.o.Log.Warn("auth: forge sign-in failed", "client", p.Client, "err", err)
		s.auditEvent(ctx, "auth.sign_in", audit.Error, "", p.Client, map[string]any{"reason": "forge exchange failed", "ui": p.UI, "device": p.Device != ""})
		if p.Device != "" || p.UI {
			page(w, 502, "Sign-in failed", "devbot couldn't complete the sign-in with the forge. Try again.")
			return
		}
		s.back(w, r, p, url.Values{"error": {"server_error"}, "error_description": {"the forge sign-in couldn't be completed"}})
		return
	}
	if p.UI {
		s.startSession(w, r, login, tok)
		return
	}
	g, err := s.newGrant(ctx, p.Client, login, tok, p.Device != "")
	ct := randomToken("")
	if err == nil {
		err = s.set(ctx, "consent:"+hash(ct), consentRec{P: p, Grant: g.ID, Login: login}, pendingTTL)
	}
	if err != nil {
		page(w, 500, "Something went wrong", "devbot couldn't record the sign-in. Try again.")
		return
	}
	// Anyone can register a client, and the forge only vouches for
	// devbot's app, not for which client asked: so the person decides here,
	// seeing the client's name and where devbot will send them.
	c, _ := s.loadClient(ctx, p.Client)
	d := pageData{Title: "Allow access to devbot", Consent: true, Token: ct, Login: login, Forge: s.o.Forge.Name()}
	if c != nil {
		d.Client = c.Name
	}
	if d.Client == "" {
		d.Client = "An unnamed app"
	}
	if p.Device != "" {
		var dr deviceRec
		s.get(ctx, "device:"+p.Device, &dr)
		d.UserCode = dr.UserCode
	} else if u, err := url.Parse(p.Redirect); err == nil {
		d.Dest = u.Scheme + "://" + u.Host
		if u.Host == "" {
			d.Dest = p.Redirect
		}
	}
	render(w, 200, d)
}

// consent is the person's answer on the consent page. Only a same-origin
// post from the browser that signed in counts.
func (s *Server) consent(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, s.o.ResourceURL) {
		page(w, 403, "Not allowed", "Approve the sign-in on devbot's own page.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	r.ParseForm()
	ctx := r.Context()
	var rec consentRec
	ok, err := s.getDel(ctx, "consent:"+hash(r.PostForm.Get("consent")), &rec)
	if err != nil || !ok || !sameBrowser(r, rec.P.Browser) {
		page(w, 400, "Sign-in expired", "This approval was already answered or has expired. Start again from your app.")
		return
	}
	p := rec.P
	if r.PostForm.Get("decision") != "allow" {
		s.auditEvent(ctx, "auth.sign_in", audit.Denied, rec.Login, p.Client, map[string]any{"reason": "consent refused", "device": p.Device != ""})
		s.Revoke(ctx, rec.Grant)
		if p.Device != "" {
			s.updateDevice(r, p.Device, func(d *deviceRec) { d.Denied = true })
			page(w, 200, "Not allowed", "The device wasn't signed in.")
			return
		}
		s.back(w, r, p, url.Values{"error": {"access_denied"}, "error_description": {"the person didn't allow this app"}})
		return
	}
	s.o.Log.Info("auth: signed in", "login", rec.Login, "provider", s.o.Forge.Name(), "client", p.Client, "device", p.Device != "")
	if p.Device != "" {
		// One sign-in per device code, however many browsers race for it.
		claimed, err := s.o.Redis.SetNX(ctx, prefix+"device-claim:"+p.Device, rec.Grant, deviceTTL).Result()
		if err != nil || !claimed || !s.updateDevice(r, p.Device, func(d *deviceRec) { d.Grant = rec.Grant }) {
			s.Revoke(ctx, rec.Grant)
			s.auditEvent(ctx, "auth.sign_in", audit.Refused, rec.Login, p.Client, map[string]any{"reason": "device code expired or already used", "device": true})
			page(w, 400, "Code expired", "That device code expired or was already used. Start the sign-in again on the device.")
			return
		}
		s.auditEvent(ctx, "auth.sign_in", audit.OK, rec.Login, p.Client, map[string]any{"device": true})
		page(w, 200, "Signed in", "The device is signed in to devbot as @"+rec.Login+". You can close this tab.")
		return
	}
	code := randomToken("devbot_code_")
	cr := codeRec{Client: p.Client, RedirectURI: p.RedirectURI, Challenge: p.Challenge, Grant: rec.Grant}
	if err := s.set(ctx, "code:"+hash(code), cr, codeTTL); err != nil {
		s.auditEvent(ctx, "auth.sign_in", audit.Error, rec.Login, p.Client, map[string]any{"reason": "couldn't store the code"})
		s.back(w, r, p, url.Values{"error": {"server_error"}})
		return
	}
	s.auditEvent(ctx, "auth.sign_in", audit.OK, rec.Login, p.Client, nil)
	s.back(w, r, p, url.Values{"code": {code}})
}

// ---- token endpoint ----

// clientAuth authenticates the client at the token, revocation and device
// endpoints: a public client by its ID, a confidential one by its secret.
func (s *Server) clientAuth(r *http.Request) (*client, bool) {
	id, secret, basic := r.BasicAuth()
	if basic {
		id, _ = url.QueryUnescape(id)
		secret, _ = url.QueryUnescape(secret)
	} else {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	c, err := s.loadClient(r.Context(), id)
	if err != nil || c == nil {
		return nil, false
	}
	if c.AuthMethod == "none" {
		return c, true
	}
	return c, secret != "" && subtle.ConstantTimeCompare([]byte(hash(secret)), []byte(c.SecretHash)) == 1
}

func parseForm(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		oauthError(w, 400, "invalid_request", "send an application/x-www-form-urlencoded body")
		return false
	}
	return true
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r) {
		return
	}
	gt, cid := r.PostForm.Get("grant_type"), r.PostForm.Get("client_id")
	fail := func(code int, errCode, desc string) {
		s.auditEvent(r.Context(), tokenEvent(gt), failure(code), "", cid, map[string]any{"grant_type": gt, "reason": errCode})
		oauthError(w, code, errCode, desc)
	}
	c, ok := s.clientAuth(r)
	if c != nil {
		cid = c.ID
	}
	if !ok {
		fail(401, "invalid_client", "unknown client or bad client credentials")
		return
	}
	f := r.PostForm
	if gt != grantCode && gt != grantRefresh && gt != grantDevice {
		fail(400, "unsupported_grant_type", "supported: authorization_code, refresh_token, device_code")
		return
	}
	if gt == grantDevice && !s.deviceGrant() {
		fail(400, "unsupported_grant_type", "this install has turned the device_code grant off")
		return
	}
	allowed := c.allows(gt)
	if gt == grantRefresh {
		// A refresh continues a sign-in made with one of the others. With
		// the device grant off, a device's earlier sign-in ends here, or
		// below if the client is registered for the code flow too.
		allowed = c.allows(grantCode) || c.allows(grantDevice) && s.deviceGrant()
	}
	if !allowed {
		fail(400, "unauthorized_client", "this client isn't registered for "+gt)
		return
	}
	if !s.resourceOK(f.Get("resource")) {
		fail(400, "invalid_target", "this server's resource is "+s.o.ResourceURL)
		return
	}
	ctx := r.Context()
	var gid, redeeming string
	switch gt {
	case grantCode:
		var rec codeRec
		found, err := s.redeemCode(ctx, hash(f.Get("code")), &rec)
		if err != nil {
			fail(500, "server_error", "")
			return
		}
		if !found || rec.Client != c.ID || rec.RedirectURI != f.Get("redirect_uri") ||
			oauth2.S256ChallengeFromVerifier(f.Get("code_verifier")) != rec.Challenge {
			fail(400, "invalid_grant", "the code is invalid, expired, already used, or its code_verifier or redirect_uri doesn't match")
			return
		}
		gid = rec.Grant
	case grantRefresh:
		rt := hash(f.Get("refresh_token"))
		// Read, not consumed: issue retires it under the grant's lock, so a
		// refresh that fails on the way doesn't cost the client its token.
		id, err := s.o.Redis.Get(ctx, prefix+"rt:"+rt).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			fail(500, "server_error", "")
			return
		}
		graced := false
		if id == "" {
			// Just rotated: within the grace window, issue answers it with
			// the tokens it was rotated into, so processes sharing a sign-in
			// that refresh at once all end up with the same ones.
			if id, err = s.graceGrant(ctx, rt); err != nil {
				fail(500, "server_error", "")
				return
			}
			graced = id != ""
		}
		if id == "" {
			// A retired refresh token again: it leaked. End its grant.
			if old, _ := s.o.Redis.Get(ctx, prefix+"rt-used:"+rt).Result(); old != "" {
				s.o.Log.Warn("auth: a rotated refresh token was reused; ending the grant")
				s.Revoke(ctx, old)
			}
			fail(400, "invalid_grant", "the refresh token is invalid, expired or revoked: sign in again")
			return
		}
		g, err := s.loadGrant(ctx, id)
		if err == nil && g != nil && g.Client != c.ID && graced {
			// The grace is for the client the token was issued to only.
			s.o.Log.Warn("auth: a rotated refresh token was reused by another client; ending the grant")
			s.Revoke(ctx, id)
		}
		if err != nil || g == nil || g.Client != c.ID {
			fail(400, "invalid_grant", "the refresh token is invalid, expired or revoked: sign in again")
			return
		}
		// The client may also be registered for the code flow, so its
		// registration alone doesn't end a device's sign-in.
		if g.Device && !s.deviceGrant() {
			fail(400, "invalid_grant", "this install has turned device sign-in off: sign in again another way")
			return
		}
		// The person's forge sign-in has to still hold, too.
		if _, err := s.forgeToken(ctx, id); errors.Is(err, ErrInvalidToken) {
			fail(400, "invalid_grant", "the forge no longer accepts this sign-in: sign in again")
			return
		} else if err != nil {
			s.o.Log.Warn("auth: refresh", "err", err)
			fail(503, "temporarily_unavailable", "devbot couldn't reach the forge; try again")
			return
		}
		gid, redeeming = id, rt
	case grantDevice:
		var ok bool
		if gid, ok = s.pollDevice(w, r, c); !ok {
			return
		}
	}
	resp, err := s.issue(ctx, gid, redeeming)
	if errors.Is(err, errGrant) || errors.Is(err, errReused) {
		fail(400, "invalid_grant", "the sign-in has ended, or this refresh token was already used: sign in again")
		return
	}
	if err != nil {
		s.o.Log.Warn("auth: issuing tokens", "err", err)
		fail(503, "temporarily_unavailable", "try again")
		return
	}
	login := ""
	if g, _ := s.loadGrant(ctx, gid); g != nil {
		login = g.Login
	}
	s.auditEvent(ctx, tokenEvent(gt), audit.OK, login, c.ID, map[string]any{"grant_type": gt})
	writeJSON(w, 200, resp)
}

// redeemScript takes a code and leaves its record behind as the code-used
// marker in one step, so no second presentation can slip between the two.
// The marker keeps the record's MAC, bound to the code: key.
var redeemScript = redis.NewScript(`
local v = redis.call('GETDEL', KEYS[1])
if v then redis.call('SET', KEYS[2], v, 'PX', ARGV[1]) end
return v`)

// redeemCode consumes an authorization code. A code presented again was
// seen by someone else too, so the grant it produced ends, with any tokens
// already issued for it (OAuth 2.1 4.1.3), as rt-used does for refresh
// tokens.
func (s *Server) redeemCode(ctx context.Context, h string, rec *codeRec) (bool, error) {
	used := prefix + "code-used:" + h
	b, err := redeemScript.Run(ctx, s.o.Redis, []string{prefix + "code:" + h, used}, usedCodeTTL.Milliseconds()).Text()
	if err == nil {
		return s.unpack(ctx, "code:"+h, []byte(b), rec)
	}
	if !errors.Is(err, redis.Nil) {
		return false, err
	}
	var old codeRec
	if b, err := s.o.Redis.Get(ctx, used).Bytes(); err == nil {
		s.unpack(ctx, "code:"+h, b, &old)
	}
	if old.Grant != "" {
		s.o.Log.Warn("auth: an authorization code was presented again; ending its grant", "client", old.Client)
		if err := s.Revoke(ctx, old.Grant); err != nil {
			return false, err
		}
	}
	return false, nil
}

// ---- revocation (RFC 7009) ----

// revoke ends the grant behind either of its tokens, deleting the person's
// stored forge tokens with it. It answers 200 whatever the token was.
func (s *Server) revoke(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r) {
		return
	}
	c, ok := s.clientAuth(r)
	if !ok {
		s.auditEvent(r.Context(), "auth.revoke", audit.Refused, "", r.PostForm.Get("client_id"), map[string]any{"reason": "invalid_client"})
		oauthError(w, 401, "invalid_client", "unknown client or bad client credentials")
		return
	}
	ctx := r.Context()
	h := hash(r.PostForm.Get("token"))
	var gid string
	var rec accessRec
	if found, _ := s.get(ctx, "at:"+h, &rec); found {
		gid = rec.Grant
	} else {
		gid, _ = s.o.Redis.Get(ctx, prefix+"rt:"+h).Result()
	}
	if gid != "" {
		if g, _ := s.loadGrant(ctx, gid); g != nil && g.Client == c.ID {
			if err := s.Revoke(ctx, gid); err != nil {
				oauthError(w, 503, "server_error", "")
				return
			}
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(200)
}

// ---- device authorization grant (RFC 8628) ----

const userCodeChars = "BCDFGHJKLMNPQRSTVWXZ" // no vowels, no lookalikes

func newUserCode() string {
	b := []byte(randomToken(""))
	out := make([]byte, 0, 9)
	for i := 0; len(out) < 9; i++ {
		if len(out) == 4 {
			out = append(out, '-')
		}
		out = append(out, userCodeChars[int(b[i])%len(userCodeChars)])
	}
	return string(out)
}

func normalizeUserCode(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if strings.ContainsRune(userCodeChars, r) {
			b.WriteRune(r)
		}
	}
	if b.Len() != 8 {
		return ""
	}
	return b.String()[:4] + "-" + b.String()[4:]
}

const pollInterval = 5 * time.Second

func (s *Server) deviceAuthorization(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r) {
		return
	}
	cid := r.PostForm.Get("client_id")
	fail := func(code int, errCode, desc string) {
		s.auditEvent(r.Context(), "auth.device.authorize", failure(code), "", cid, map[string]any{"reason": errCode})
		oauthError(w, code, errCode, desc)
	}
	c, ok := s.clientAuth(r)
	if c != nil {
		cid = c.ID
	}
	if !ok {
		fail(401, "invalid_client", "unknown client or bad client credentials")
		return
	}
	if !s.deviceGrant() {
		fail(400, "unauthorized_client", "this install has turned the device_code grant off")
		return
	}
	if !c.allows(grantDevice) {
		fail(400, "unauthorized_client", "this client isn't registered for the device_code grant")
		return
	}
	if !s.resourceOK(r.PostForm.Get("resource")) {
		fail(400, "invalid_target", "this server's resource is "+s.o.ResourceURL)
		return
	}
	ctx := r.Context()
	dc, uc := randomToken("devbot_dc_"), newUserCode()
	ok, err := s.o.Redis.SetNX(ctx, prefix+"usercode:"+uc, hash(dc), deviceTTL).Result()
	if err == nil && !ok {
		fail(503, "temporarily_unavailable", "try again")
		return
	}
	if err == nil {
		err = s.set(ctx, "device:"+hash(dc), deviceRec{Client: c.ID, UserCode: uc}, deviceTTL)
	}
	if err != nil {
		fail(500, "server_error", "")
		return
	}
	s.auditEvent(ctx, "auth.device.authorize", audit.OK, "", c.ID, nil)
	// No verification_uri_complete: a link with the code in it is exactly
	// what a phisher sends (RFC 8628 5.4). The person types the code from
	// the device in front of them.
	writeJSON(w, 200, map[string]any{
		"device_code": dc, "user_code": uc, "verification_uri": s.url("/oauth/device"),
		"expires_in": int(deviceTTL.Seconds()), "interval": int(pollInterval.Seconds()),
	})
}

func (s *Server) updateDevice(r *http.Request, dh string, f func(*deviceRec)) bool {
	var d deviceRec
	if ok, err := s.get(r.Context(), "device:"+dh, &d); !ok || err != nil {
		return false
	}
	f(&d)
	return s.keepTTL(r.Context(), "device:"+dh, d) == nil
}

// pollDevice answers a device's token request: still waiting, too fast,
// denied, expired, or approved with the grant to issue tokens for.
func (s *Server) pollDevice(w http.ResponseWriter, r *http.Request, c *client) (string, bool) {
	ctx := r.Context()
	dh := hash(r.PostForm.Get("device_code"))
	var d deviceRec
	found, err := s.get(ctx, "device:"+dh, &d)
	switch {
	case err != nil:
		oauthError(w, 500, "server_error", "")
		return "", false
	case !found:
		oauthError(w, 400, "expired_token", "the device code expired: start again")
		return "", false
	case d.Client != c.ID:
		oauthError(w, 400, "invalid_grant", "the device code belongs to another client")
		return "", false
	case d.Denied:
		s.o.Redis.Del(ctx, prefix+"device:"+dh, prefix+"usercode:"+d.UserCode)
		oauthError(w, 400, "access_denied", "the sign-in was not approved")
		return "", false
	case d.Grant != "":
		// Exactly one poll gets the tokens.
		if n, _ := s.o.Redis.Del(ctx, prefix+"device:"+dh).Result(); n != 1 {
			oauthError(w, 400, "expired_token", "the device code was already used")
			return "", false
		}
		s.o.Redis.Del(ctx, prefix+"usercode:"+d.UserCode)
		return d.Grant, true
	}
	now := s.o.Now()
	tooFast := !d.LastPoll.IsZero() && now.Sub(d.LastPoll) < pollInterval
	d.LastPoll = now
	s.keepTTL(ctx, "device:"+dh, d)
	if tooFast {
		oauthError(w, 400, "slow_down", "poll no more often than the interval")
	} else {
		oauthError(w, 400, "authorization_pending", "waiting for the person to sign in")
	}
	return "", false
}

// Device-code entry is rate-limited, so a live code can't be found by
// guessing and signed in as someone else: each address may get deviceIPFails
// codes wrong per deviceTTL, and each code may be submitted deviceCodeTries
// times before it is locked out and the device has to start again.
const (
	deviceIPFails   = 10
	deviceCodeTries = 5
)

// devicePage never fills in a code from the link: a pre-filled code is
// someone else's sign-in, sent to the person to approve.
func (s *Server) devicePage(w http.ResponseWriter, r *http.Request) {
	if !s.deviceGrant() {
		page(w, 404, "Device sign-in is off", "This devbot install doesn't sign in devices.")
		return
	}
	deviceForm(w, 200, "")
}

// deviceSubmit takes the code the person typed and sends them to the forge
// to sign in. Only a same-origin form post counts, so another site can't
// push someone's browser through this with its own code.
func (s *Server) deviceSubmit(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, s.o.ResourceURL) {
		page(w, 403, "Not allowed", "Open the sign-in page directly and enter the code there.")
		return
	}
	if !s.deviceGrant() {
		page(w, 404, "Device sign-in is off", "This devbot install doesn't sign in devices.")
		return
	}
	ctx := r.Context()
	// Without a client address there is no per-address limit: one counter
	// shared by everyone would let anyone turn everyone away. The per-code
	// lockout still holds.
	ip := s.clientIP(r)
	wrong := func() {
		if ip != "" {
			s.count(ctx, "device-fails:"+ip, deviceTTL)
		}
	}
	if ip != "" {
		if n, _ := s.o.Redis.Get(ctx, prefix+"device-fails:"+ip).Int(); n >= deviceIPFails {
			deviceForm(w, http.StatusTooManyRequests, "Too many wrong codes from here. Wait a few minutes, then try again.")
			return
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	r.ParseForm()
	uc := normalizeUserCode(r.PostForm.Get("user_code"))
	dh, _ := s.o.Redis.Get(ctx, prefix+"usercode:"+uc).Result()
	if uc != "" && s.count(ctx, "device-tries:"+uc, deviceTTL) > deviceCodeTries {
		// Locked out: the device's next poll hears it was denied.
		if dh != "" {
			s.updateDevice(r, dh, func(d *deviceRec) { d.Denied = true })
			s.o.Redis.Del(ctx, prefix+"usercode:"+uc)
		}
		wrong()
		deviceForm(w, http.StatusTooManyRequests, "That code was tried too many times and no longer works. Start the sign-in again on the device.")
		return
	}
	var d deviceRec
	// The usercode: pointer carries no MAC, so the device record it points
	// to has to name the code too.
	if found, _ := s.get(ctx, "device:"+dh, &d); uc == "" || dh == "" || !found || d.UserCode != uc || d.Grant != "" || d.Denied {
		wrong()
		deviceForm(w, http.StatusBadRequest, "That code isn't valid, or has expired. Check the device for the current code.")
		return
	}
	s.toForge(w, r, pending{Client: d.Client, Device: dh, Verifier: oauth2.GenerateVerifier()})
}

// count adds one to a counter that starts its window of ttl at the first.
func (s *Server) count(ctx context.Context, key string, ttl time.Duration) int64 {
	n, err := s.o.Redis.Incr(ctx, prefix+key).Result()
	if err == nil && n == 1 {
		s.o.Redis.Expire(ctx, prefix+key, ttl)
	}
	return n
}

// clientIP is the address the request came from, or "" when it can't be
// told. From a trusted proxy it is the nearest X-Forwarded-For hop that
// isn't one: earlier hops are whatever the client chose to send. A proxy
// that names no client gives "", not its own address, which would make the
// per-address limit one limit for everyone behind it.
func (s *Server) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return ""
	}
	if !s.trusted(addr) {
		return addr.Unmap().String()
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			return ""
		}
		if !s.trusted(a) {
			return a.Unmap().String()
		}
	}
	return ""
}

func (s *Server) trusted(a netip.Addr) bool {
	a = a.Unmap()
	for _, p := range s.o.TrustedProxies {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func sameOrigin(r *http.Request, base string) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return false
	}
	o := r.Header.Get("Origin")
	if o == "" || o == "null" {
		return r.Header.Get("Sec-Fetch-Site") != "" // a browser that sent fetch metadata, and it was fine
	}
	b, err := url.Parse(base)
	return err == nil && strings.EqualFold(o, b.Scheme+"://"+b.Host)
}
