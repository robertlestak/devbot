// Package slack is devbot's Slack app: its install into each workspace
// (Slack's OAuth v2 flow), the Events API, interactivity and slash command
// endpoints, and a client of Slack's Web API.
//
// Every request Slack sends is checked against the app's signing secret
// before anything else reads it. Slack retries a delivery it didn't see
// answered in time, and every serve replica takes them, so each event and
// interaction is claimed once in Redis before it is handled.
//
// A workspace's bot token is sealed with the auth key and kept in Redis,
// like people's forge tokens. Like them it doesn't survive a Redis wipe:
// the workspace installs the app again.
package slack

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"git.shdw.tech/rob/devbot/internal/audit"
	"git.shdw.tech/rob/devbot/internal/auth"
)

const (
	prefix   = "devbot:slack:"
	stateTTL = 10 * time.Minute // a person's time on Slack's install page
	// Slack retries a delivery three times within about five minutes.
	claimTTL = time.Hour
	// MaxAge is how old a request's timestamp may be, either way, before
	// it is refused as a possible replay.
	MaxAge = 5 * time.Minute

	installCookie = "devbot_slack_install"
	callbackPath  = "/slack/oauth/callback"
)

// Scopes are the bot scopes the app asks each workspace for.
var Scopes = []string{"app_mentions:read", "chat:write", "commands", "users:read", "channels:history", "groups:history", "im:history", "mpim:history"}

type Options struct {
	ClientID, ClientSecret string
	// SigningSecret is what Slack signs every request it sends with.
	SigningSecret string
	// ResourceURL is devbot's public base URL; Slack sends installs back to
	// its /slack/oauth/callback.
	ResourceURL string
	// AllowedTeams are the workspace or enterprise IDs that may install.
	AllowedTeams []string
	Redis        *redis.Client
	// Sealer encrypts bot tokens at rest: the auth key's.
	Sealer *auth.Sealer
	// MaxBody is the largest request body read.
	MaxBody int64
	Log     *slog.Logger
	// Audit records installs and uninstalls. Nil records nothing.
	Audit *audit.Logger
	// APIURL is Slack's Web API base, AuthorizeURL its install page,
	// OIDCURL its Sign in with Slack page and HooksURL where its
	// response_urls are; tests point them at a fake.
	APIURL, AuthorizeURL, OIDCURL, HooksURL string
	HTTP                                    *http.Client
	// NewAPI is the Web API client for a bot token. Default: a Client.
	NewAPI func(token string) API
	// Now is the clock, for tests.
	Now func() time.Time
}

// App is the Slack app's HTTP side. Register handlers (OnEvent,
// OnInteraction, OnCommand, OnUninstall) before serving its routes.
type App struct {
	o           Options
	events      map[string]EventHandler
	interaction InteractionHandler
	command     CommandHandler
	uninstall   []func(context.Context, Workspace) error
	// cutOff are the workspaces already logged as no longer allowed.
	cutOff sync.Map
}

func New(o Options) (*App, error) {
	if o.ClientID == "" || o.ClientSecret == "" || o.SigningSecret == "" || o.ResourceURL == "" || o.Redis == nil || o.Sealer == nil {
		return nil, errors.New("slack: client ID and secret, signing secret, resource URL, Redis and the auth key are required")
	}
	o.ResourceURL = strings.TrimRight(o.ResourceURL, "/")
	if o.APIURL == "" {
		o.APIURL = "https://slack.com/api/"
	}
	if o.AuthorizeURL == "" {
		o.AuthorizeURL = "https://slack.com/oauth/v2/authorize"
	}
	if o.OIDCURL == "" {
		o.OIDCURL = "https://slack.com/openid/connect/authorize"
	}
	if o.HooksURL == "" {
		o.HooksURL = "https://hooks.slack.com/"
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	if o.NewAPI == nil {
		o.NewAPI = func(token string) API { return &Client{Token: token, URL: o.APIURL, HTTP: o.HTTP} }
	}
	if o.MaxBody == 0 {
		o.MaxBody = 1 << 20
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	a := &App{o: o, events: map[string]EventHandler{}}
	a.events["app_uninstalled"] = func(ctx context.Context, e Event) error { return a.Uninstall(ctx, e.Workspace, e.Type) }
	a.events["tokens_revoked"] = func(ctx context.Context, e Event) error {
		var ev struct {
			Tokens struct {
				Bot []string `json:"bot"`
			} `json:"tokens"`
		}
		json.Unmarshal(e.Raw, &ev)
		// The app keeps only bot tokens; a user's revoked token isn't one.
		if len(ev.Tokens.Bot) == 0 {
			return nil
		}
		return a.Uninstall(ctx, e.Workspace, e.Type)
	}
	return a, nil
}

// Routes registers the app's endpoints on mux.
func (a *App) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /slack/install", a.install)
	mux.HandleFunc("GET "+callbackPath, a.callback)
	mux.HandleFunc("POST /slack/events", stamped(a.eventsEndpoint))
	mux.HandleFunc("POST /slack/interactivity", stamped(a.interactivity))
	mux.HandleFunc("POST /slack/commands", stamped(a.commands))
}

type receivedKey struct{}

// stamped has h's requests carry when serve received them, before the
// body is read and verified: Slack's 3 seconds count that time too.
func stamped(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h(w, r.WithContext(context.WithValue(r.Context(), receivedKey{}, time.Now())))
	}
}

// Received is when serve received the Slack request ctx is for: zero for
// a context that isn't one's.
func Received(ctx context.Context) time.Time {
	t, _ := ctx.Value(receivedKey{}).(time.Time)
	return t
}

// Workspace is where the app is installed: a workspace (Team), within an
// Enterprise Grid org when Enterprise is set, or with an org-wide install
// the whole org (Enterprise, and no Team).
type Workspace struct {
	Enterprise string `json:"enterprise,omitempty"`
	Team       string `json:"team,omitempty"`
}

func (w Workspace) key() string { return w.Enterprise + ":" + w.Team }

func (w Workspace) String() string {
	if w.Team == "" {
		return w.Enterprise
	}
	return w.Team
}

// allowed reports whether ws may install: its workspace or its org is
// listed. An empty list allows none.
func (a *App) allowed(ws Workspace) bool {
	return (ws.Team != "" && slices.Contains(a.o.AllowedTeams, ws.Team)) ||
		(ws.Enterprise != "" && slices.Contains(a.o.AllowedTeams, ws.Enterprise))
}

// admitted reports whether ws is still allowed: a workspace taken off the
// list after it installed is treated as not installed, counted by where it
// was turned away (use), and logged once per replica.
func (a *App) admitted(ws Workspace, use string) bool {
	if a.allowed(ws) {
		return true
	}
	metrics.add("devbot_slack_disallowed_total", "use", use)
	if _, seen := a.cutOff.LoadOrStore(ws.key(), true); !seen {
		a.o.Log.Warn("slack: ignoring a workspace that isn't in slack.allowed_teams", "workspace", ws.String(), "enterprise", ws.Enterprise)
	}
	return false
}

// Installation is the app's install in one workspace.
type Installation struct {
	Workspace
	TeamName    string    `json:"team_name,omitempty"`
	AppID       string    `json:"app_id,omitempty"`
	BotUserID   string    `json:"bot_user_id,omitempty"`
	Scopes      string    `json:"scopes,omitempty"`
	InstalledBy string    `json:"installed_by,omitempty"` // the Slack user who installed it
	InstalledAt time.Time `json:"installed_at"`
	Token       string    `json:"token"`
}

// ErrNotInstalled is a workspace the app has no token for.
var ErrNotInstalled = errors.New("slack: the app isn't installed in this workspace")

func installKey(ws Workspace) string { return prefix + "install:" + ws.key() }

// installAAD binds a sealed installation to its workspace, so a record
// can't be moved to another one.
func installAAD(ws Workspace) string { return "slack install\x00" + ws.key() }

// Save stores in as its workspace's install, replacing any before it. The
// install callback is what calls it; tests do too.
func (a *App) Save(ctx context.Context, in Installation) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return a.o.Redis.Set(ctx, installKey(in.Workspace), a.o.Sealer.Seal(b, installAAD(in.Workspace)), 0).Err()
}

// Installation is ws's install, or ErrNotInstalled, also for a workspace
// no longer in slack.allowed_teams.
func (a *App) Installation(ctx context.Context, ws Workspace) (Installation, error) {
	if !a.admitted(ws, "token") {
		return Installation{}, ErrNotInstalled
	}
	sealed, err := a.o.Redis.Get(ctx, installKey(ws)).Result()
	if errors.Is(err, redis.Nil) {
		return Installation{}, ErrNotInstalled
	} else if err != nil {
		return Installation{}, err
	}
	b, stale, err := a.o.Sealer.Open(sealed, installAAD(ws))
	if err != nil {
		return Installation{}, fmt.Errorf("slack: workspace %s: %w", ws, err)
	}
	var in Installation
	if err := json.Unmarshal(b, &in); err != nil {
		return Installation{}, err
	}
	if stale {
		// Sealed with the previous auth key: seal it under the current one.
		if err := a.Save(ctx, in); err != nil {
			a.o.Log.Warn("slack: re-sealing a workspace's token", "workspace", ws.String(), "err", err)
		}
	}
	return in, nil
}

// Bot is the Web API as ws's bot.
func (a *App) Bot(ctx context.Context, ws Workspace) (API, error) {
	in, err := a.Installation(ctx, ws)
	if err != nil {
		return nil, err
	}
	return a.o.NewAPI(in.Token), nil
}

// OnUninstall adds f to what Uninstall runs: whatever else devbot keeps
// for a workspace goes with its token.
func (a *App) OnUninstall(f func(context.Context, Workspace) error) {
	a.uninstall = append(a.uninstall, f)
}

// Uninstall forgets ws: its bot token, and through the OnUninstall hooks
// everything else kept for it. Nothing left is not an error, so it is
// safe to run again.
func (a *App) Uninstall(ctx context.Context, ws Workspace, reason string) error {
	n, err := a.o.Redis.Del(ctx, installKey(ws)).Result()
	if err != nil {
		return err
	}
	if err := a.unbind(ctx, ws); err != nil {
		return err
	}
	for _, f := range a.uninstall {
		if err := f(ctx, ws); err != nil {
			a.o.Audit.Log(ctx, audit.Record{Event: "slack.uninstall", Outcome: audit.Error, ActorKind: "devbot", Err: err,
				Detail: map[string]any{"team_id": ws.Team, "enterprise_id": ws.Enterprise, "reason": reason}})
			return err
		}
	}
	a.o.Audit.Log(ctx, audit.Record{Event: "slack.uninstall", ActorKind: "devbot",
		Detail: map[string]any{"team_id": ws.Team, "enterprise_id": ws.Enterprise, "reason": reason, "had_token": n > 0}})
	metrics.add("devbot_slack_uninstalls_total", "reason", reason)
	return nil
}

// ---- install ----

func randomState() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func stateKey(state string) string {
	h := sha256.Sum256([]byte(state))
	return prefix + "state:" + hex.EncodeToString(h[:])
}

// install sends a person to Slack's install page. The state is single-use
// and bound to their browser by a cookie, so nobody can finish an install
// someone else started.
func (a *App) install(w http.ResponseWriter, r *http.Request) {
	state := randomState()
	if err := a.o.Redis.Set(r.Context(), stateKey(state), 1, stateTTL).Err(); err != nil {
		a.o.Log.Warn("slack install: storing the state", "err", err)
		http.Error(w, "devbot can't start an install right now; try again", http.StatusServiceUnavailable)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: installCookie, Value: state, Path: callbackPath, MaxAge: int(stateTTL.Seconds()),
		HttpOnly: true, Secure: strings.HasPrefix(a.o.ResourceURL, "https://"), SameSite: http.SameSiteLaxMode})
	q := url.Values{"client_id": {a.o.ClientID}, "scope": {strings.Join(Scopes, ",")}, "state": {state}, "redirect_uri": {a.o.ResourceURL + callbackPath}}
	http.Redirect(w, r, a.o.AuthorizeURL+"?"+q.Encode(), http.StatusFound)
}

// page answers a person's browser with plain text.
func page(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	io.WriteString(w, msg+"\n")
}

type accessResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	Scope       string `json:"scope"`
	BotUserID   string `json:"bot_user_id"`
	AppID       string `json:"app_id"`
	Team        *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"team"`
	Enterprise *struct {
		ID string `json:"id"`
	} `json:"enterprise"`
	IsEnterpriseInstall bool `json:"is_enterprise_install"`
	AuthedUser          struct {
		ID string `json:"id"`
	} `json:"authed_user"`
}

func (a *App) callback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	state := q.Get("state")
	c, err := r.Cookie(installCookie)
	http.SetCookie(w, &http.Cookie{Name: installCookie, Path: callbackPath, MaxAge: -1})
	if state == "" || err != nil || !hmac.Equal([]byte(c.Value), []byte(state)) {
		page(w, http.StatusBadRequest, "This install wasn't started from this browser. Start again from devbot's /slack/install.")
		return
	}
	if n, err := a.o.Redis.Del(ctx, stateKey(state)).Result(); err != nil || n == 0 {
		page(w, http.StatusBadRequest, "This install link has expired or was used already. Start again from devbot's /slack/install.")
		return
	}
	if e := q.Get("error"); e != "" {
		page(w, http.StatusBadRequest, "Slack didn't install devbot: "+e)
		return
	}
	var resp accessResponse
	client := &Client{URL: a.o.APIURL, HTTP: a.o.HTTP}
	form := url.Values{"code": {q.Get("code")}, "redirect_uri": {a.o.ResourceURL + callbackPath}}
	if err := client.callBasic(ctx, "oauth.v2.access", a.o.ClientID, a.o.ClientSecret, form, &resp); err != nil {
		a.installed(ctx, Workspace{}, "", audit.Error, err)
		page(w, http.StatusBadGateway, "Slack didn't complete the install. Start again from devbot's /slack/install.")
		return
	}
	var ws Workspace
	if resp.Enterprise != nil {
		ws.Enterprise = resp.Enterprise.ID
	}
	if resp.Team != nil && !resp.IsEnterpriseInstall {
		ws.Team = resp.Team.ID
	}
	if resp.AccessToken == "" || resp.TokenType != "bot" || ws == (Workspace{}) {
		a.installed(ctx, ws, resp.AuthedUser.ID, audit.Error, errors.New("oauth.v2.access returned no bot token for a workspace"))
		page(w, http.StatusBadGateway, "Slack didn't complete the install. Start again from devbot's /slack/install.")
		return
	}
	if !a.allowed(ws) {
		// Slack has installed the app there all the same: revoking its only
		// token takes it out again.
		bot := &Client{Token: resp.AccessToken, URL: a.o.APIURL, HTTP: a.o.HTTP}
		if err := bot.call(ctx, "auth.revoke", url.Values{}, nil); err != nil {
			a.o.Log.Warn("slack install: revoking a refused workspace's token", "workspace", ws.String(), "err", err)
		}
		a.installed(ctx, ws, resp.AuthedUser.ID, audit.Refused, errors.New("workspace isn't in slack.allowed_teams"))
		page(w, http.StatusForbidden, "This Slack workspace isn't allowed to install devbot. Ask devbot's operator to add "+ws.String()+" to slack.allowed_teams.")
		return
	}
	in := Installation{Workspace: ws, AppID: resp.AppID, BotUserID: resp.BotUserID, Scopes: resp.Scope,
		InstalledBy: resp.AuthedUser.ID, InstalledAt: a.o.Now().UTC(), Token: resp.AccessToken}
	if resp.Team != nil {
		in.TeamName = resp.Team.Name
	}
	if err := a.Save(ctx, in); err != nil {
		a.installed(ctx, ws, resp.AuthedUser.ID, audit.Error, err)
		page(w, http.StatusServiceUnavailable, "devbot couldn't store the install; start again from devbot's /slack/install.")
		return
	}
	a.installed(ctx, ws, resp.AuthedUser.ID, audit.OK, nil)
	page(w, http.StatusOK, "devbot is installed in this Slack workspace. You can close this page.")
}

// installed audits and counts an install's outcome.
func (a *App) installed(ctx context.Context, ws Workspace, user, outcome string, err error) {
	metrics.add("devbot_slack_installs_total", "result", outcome)
	if err != nil {
		a.o.Log.Warn("slack install", "workspace", ws.String(), "outcome", outcome, "err", err)
	}
	a.o.Audit.Log(ctx, audit.Record{Event: "slack.install", Outcome: outcome, ActorKind: "devbot", Err: err,
		Detail: map[string]any{"team_id": ws.Team, "enterprise_id": ws.Enterprise, "slack_user": user}})
}

// ---- events and interactivity ----

// Event is one Events API event_callback.
type Event struct {
	ID        string
	Workspace Workspace
	Type      string // the inner event's type
	// Raw is the inner event, as Slack sent it.
	Raw json.RawMessage
	// Retry is X-Slack-Retry-Num: 0 on the first delivery.
	Retry int
}

// EventHandler handles an event. An error has Slack deliver it again.
type EventHandler func(context.Context, Event) error

// OnEvent has h handle events of type typ, each once however often Slack
// delivers it.
func (a *App) OnEvent(typ string, h EventHandler) { a.events[typ] = h }

// Interaction is one interactivity payload: a button, a shortcut, a modal
// submitted.
type Interaction struct {
	Type      string
	TriggerID string
	Workspace Workspace
	User      string
	Raw       json.RawMessage
}

// InteractionHandler handles an interaction. A non-nil reply is sent back
// as JSON (a view_submission's response_action, say).
type InteractionHandler func(context.Context, Interaction) (reply any, err error)

// OnInteraction has h handle every interaction.
func (a *App) OnInteraction(h InteractionHandler) { a.interaction = h }

var (
	errUnsigned  = errors.New("no Slack signature")
	errSignature = errors.New("bad Slack signature")
	errStale     = errors.New("timestamp outside the last 5 minutes")
)

// verify checks Slack's v0 signature of body, and that it was made in the
// last MaxAge, so a captured request can't be replayed later.
func (a *App) verify(h http.Header, body []byte) error {
	ts, sig := h.Get("X-Slack-Request-Timestamp"), h.Get("X-Slack-Signature")
	if ts == "" || sig == "" {
		return errUnsigned
	}
	n, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return errSignature
	}
	if d := a.o.Now().Sub(time.Unix(n, 0)); d > MaxAge || d < -MaxAge {
		return errStale
	}
	mac := hmac.New(sha256.New, []byte(a.o.SigningSecret))
	fmt.Fprintf(mac, "v0:%s:", ts)
	mac.Write(body)
	if !hmac.Equal([]byte(sig), []byte("v0="+hex.EncodeToString(mac.Sum(nil)))) {
		return errSignature
	}
	return nil
}

// verified reads and verifies a request to endpoint, answering it when it
// fails: nothing about an unverified request is acted on.
func (a *App) verified(w http.ResponseWriter, r *http.Request, endpoint string) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, a.o.MaxBody))
	if err == nil {
		err = a.verify(r.Header, body)
	}
	if err != nil {
		metrics.add("devbot_slack_requests_total", "endpoint", endpoint, "result", "rejected")
		a.o.Log.Debug("slack request rejected", "endpoint", endpoint, "err", err, "remote", r.RemoteAddr)
		http.Error(w, "rejected", http.StatusUnauthorized)
		return nil, false
	}
	metrics.add("devbot_slack_requests_total", "endpoint", endpoint, "result", "verified")
	return body, true
}

// claim marks a delivery as taken, across replicas, and reports whether
// this is the first; release undoes it for one that wasn't handled.
func (a *App) claim(ctx context.Context, id string) (bool, error) {
	return a.o.Redis.SetNX(ctx, prefix+"seen:"+id, 1, claimTTL).Result()
}

func (a *App) release(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.o.Redis.Del(ctx, prefix+"seen:"+id).Err(); err != nil {
		a.o.Log.Warn("slack: unmarking a delivery", "id", id, "err", err)
	}
}

func (a *App) eventsEndpoint(w http.ResponseWriter, r *http.Request) {
	body, ok := a.verified(w, r, "events")
	if !ok {
		return
	}
	var env struct {
		Type                string          `json:"type"`
		Challenge           string          `json:"challenge"`
		TeamID              string          `json:"team_id"`
		EnterpriseID        string          `json:"enterprise_id"`
		IsEnterpriseInstall bool            `json:"is_enterprise_install"`
		EventID             string          `json:"event_id"`
		Event               json.RawMessage `json:"event"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		http.Error(w, "not JSON", http.StatusBadRequest)
		return
	}
	switch env.Type {
	case "url_verification":
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"challenge": env.Challenge})
		return
	case "event_callback":
	default:
		w.WriteHeader(http.StatusOK)
		return
	}
	var inner struct {
		Type string `json:"type"`
	}
	json.Unmarshal(env.Event, &inner)
	h := a.events[inner.Type]
	if h == nil || env.EventID == "" {
		w.WriteHeader(http.StatusOK)
		return
	}
	ws := Workspace{Enterprise: env.EnterpriseID, Team: env.TeamID}
	if env.IsEnterpriseInstall {
		ws.Team = ""
	}
	// A workspace taken off the list still gets its uninstall, which only
	// deletes what is kept for it. Nothing else is handled, and it is
	// answered 200 so Slack doesn't retry.
	if inner.Type != "app_uninstalled" && inner.Type != "tokens_revoked" && !a.admitted(ws, "events") {
		w.WriteHeader(http.StatusOK)
		return
	}
	retry, _ := strconv.Atoi(r.Header.Get("X-Slack-Retry-Num"))
	id := "event:" + env.EventID
	if first, err := a.claim(r.Context(), id); err != nil {
		// Without Redis there is no telling a retry apart: Slack sends it
		// again.
		a.o.Log.Warn("slack event: claiming", "event_id", env.EventID, "err", err)
		http.Error(w, "try again", http.StatusServiceUnavailable)
		return
	} else if !first {
		metrics.add("devbot_slack_duplicates_total", "endpoint", "events")
		w.WriteHeader(http.StatusOK)
		return
	}
	if err := h(r.Context(), Event{ID: env.EventID, Workspace: ws, Type: inner.Type, Raw: env.Event, Retry: retry}); err != nil {
		a.release(id)
		a.o.Log.Warn("slack event", "type", inner.Type, "event_id", env.EventID, "workspace", ws.String(), "err", err)
		http.Error(w, "failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (a *App) interactivity(w http.ResponseWriter, r *http.Request) {
	body, ok := a.verified(w, r, "interactivity")
	if !ok {
		return
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		http.Error(w, "not a form", http.StatusBadRequest)
		return
	}
	raw := form.Get("payload")
	var p struct {
		Type      string `json:"type"`
		TriggerID string `json:"trigger_id"`
		Team      *struct {
			ID string `json:"id"`
		} `json:"team"`
		Enterprise *struct {
			ID string `json:"id"`
		} `json:"enterprise"`
		IsEnterpriseInstall bool `json:"is_enterprise_install"`
		User                struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		http.Error(w, "no payload", http.StatusBadRequest)
		return
	}
	var ws Workspace
	if p.Enterprise != nil {
		ws.Enterprise = p.Enterprise.ID
	}
	if p.Team != nil && !p.IsEnterpriseInstall {
		ws.Team = p.Team.ID
	}
	if a.interaction == nil || !a.admitted(ws, "interactivity") {
		w.WriteHeader(http.StatusOK)
		return
	}
	var id string
	if p.TriggerID != "" {
		id = "trigger:" + p.TriggerID
		if first, err := a.claim(r.Context(), id); err != nil {
			a.o.Log.Warn("slack interaction: claiming", "err", err)
			http.Error(w, "try again", http.StatusServiceUnavailable)
			return
		} else if !first {
			metrics.add("devbot_slack_duplicates_total", "endpoint", "interactivity")
			w.WriteHeader(http.StatusOK)
			return
		}
	}
	reply, err := a.interaction(r.Context(), Interaction{Type: p.Type, TriggerID: p.TriggerID, Workspace: ws, User: p.User.ID, Raw: json.RawMessage(raw)})
	if err != nil {
		if id != "" {
			a.release(id)
		}
		a.o.Log.Warn("slack interaction", "type", p.Type, "workspace", ws.String(), "err", err)
		http.Error(w, "failed", http.StatusInternalServerError)
		return
	}
	if reply == nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(reply)
}

// Command is one invocation of the app's slash command.
type Command struct {
	Workspace Workspace
	User      string
	Channel   string
	Command   string // "/devbot"
	Text      string // what follows it
	TriggerID string
	// ResponseURL answers the command later, for up to 30 minutes (see
	// Respond).
	ResponseURL string
}

// CommandHandler handles a command. A non-nil reply is sent back as JSON:
// a message, only the user sees unless it says otherwise.
type CommandHandler func(context.Context, Command) (reply any, err error)

// OnCommand has h handle every slash command.
func (a *App) OnCommand(h CommandHandler) { a.command = h }

func (a *App) commands(w http.ResponseWriter, r *http.Request) {
	body, ok := a.verified(w, r, "commands")
	if !ok {
		return
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		http.Error(w, "not a form", http.StatusBadRequest)
		return
	}
	ws := Workspace{Enterprise: form.Get("enterprise_id"), Team: form.Get("team_id")}
	if form.Get("is_enterprise_install") == "true" {
		ws.Team = ""
	}
	if a.command == nil || !a.admitted(ws, "commands") {
		w.WriteHeader(http.StatusOK)
		return
	}
	cmd := Command{Workspace: ws, User: form.Get("user_id"), Channel: form.Get("channel_id"), Command: form.Get("command"),
		Text: strings.TrimSpace(form.Get("text")), TriggerID: form.Get("trigger_id"), ResponseURL: form.Get("response_url")}
	// Slack doesn't retry a command, but a replica can't tell that a
	// request isn't a replay of one within MaxAge either.
	if cmd.TriggerID != "" {
		if first, err := a.claim(r.Context(), "trigger:"+cmd.TriggerID); err != nil {
			a.o.Log.Warn("slack command: claiming", "err", err)
			http.Error(w, "try again", http.StatusServiceUnavailable)
			return
		} else if !first {
			metrics.add("devbot_slack_duplicates_total", "endpoint", "commands")
			w.WriteHeader(http.StatusOK)
			return
		}
	}
	reply, err := a.command(r.Context(), cmd)
	if err != nil {
		if cmd.TriggerID != "" {
			a.release("trigger:" + cmd.TriggerID)
		}
		a.o.Log.Warn("slack command", "command", cmd.Command, "workspace", ws.String(), "err", err)
		http.Error(w, "failed", http.StatusInternalServerError)
		return
	}
	if reply == nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(reply)
}
