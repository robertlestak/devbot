package github

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"git.shdw.tech/rob/devbot/internal/forge"
)

// inventoryTTL is how long the repo inventory is used before it is listed
// again: installation webhooks invalidate it sooner, and the TTL bounds how
// long a lost one goes unnoticed.
const inventoryTTL = time.Hour

// inventoryRetry is how soon a refresh that failed, in whole or in part, is
// tried again. Longer than a reconcile pass takes, so a pass lists the
// installations and their repos at most once even while GitHub fails.
const inventoryRetry = 10 * time.Minute

// inventory is every installation of one App, with each one's repos: what
// reconcile searches in. Listing an installation's repos takes a request
// per hundred, so it is kept between passes, not listed per search.
type inventory struct {
	// stale is set by an installation webhook delivered to this process.
	stale atomic.Bool

	// mu is held across a refresh, so the searches that come in meanwhile
	// wait for it and use its answer rather than list again.
	mu       sync.Mutex
	installs []installed
	next     time.Time // when to list again; zero before the first list
	gen      string    // the shared generation the last list was made under
}

// installed is one installation and the repos devbot can work in through
// it: those repos.allow/deny lets it read.
type installed struct {
	installation
	// account is the login of the user or org the App is installed on, and
	// org whether it is an org, for the search qualifier.
	account string
	org     bool
	repos   map[string]installRepo // by full name
}

type installRepo struct {
	FullName  string `json:"full_name"`
	HasIssues bool   `json:"has_issues"`
}

// invalidateInventory makes the next search list the installations and
// their repos again, in this process and, through the shared generation,
// in the one reconciling.
func (c *Client) invalidateInventory(ctx context.Context) error {
	c.impl.inv.stale.Store(true)
	if c.cfg.BumpInventory == nil {
		return nil
	}
	return c.cfg.BumpInventory(ctx)
}

// inventory is a's installations and their repos: as last listed, or
// listed now when that is due (see inventoryTTL), the installation
// webhooks made it stale, or there is no list yet. A refresh that fails
// keeps what the last one knew: an installation it can't list keeps the
// repos it had, and a failure to list the installations keeps them all. The
// error comes back, once, from the call that refreshed, with the inventory
// it could build.
func (c *Client) inventory(ctx context.Context, a *app) ([]installed, error) {
	gen, shared := "", false
	if c.cfg.InventoryGeneration != nil {
		// Unreadable, the TTL and this process's webhooks alone apply.
		if g, err := c.cfg.InventoryGeneration(ctx); err == nil {
			gen, shared = g, true
		}
	}
	inv := &a.inv
	inv.mu.Lock()
	defer inv.mu.Unlock()
	stale := inv.stale.Swap(false)
	if !stale && !inv.next.IsZero() && c.now().Before(inv.next) && (!shared || gen == inv.gen) {
		return inv.installs, nil
	}
	installs, err := c.listInventory(ctx, a, inv.installs)
	inv.installs = installs
	if shared {
		inv.gen = gen
	}
	inv.next = c.now().Add(inventoryTTL)
	if err != nil {
		inv.next = c.now().Add(inventoryRetry)
	}
	return installs, err
}

// listInventory lists a's installations (GET /app/installations) and each
// one's repos (GET /installation/repositories), every page of them however
// many. prev is the last inventory, kept for what can't be listed now.
func (c *Client) listInventory(ctx context.Context, a *app, prev []installed) ([]installed, error) {
	jwt, err := c.appCred(a)
	if err != nil {
		return prev, err
	}
	installs, err := list[struct {
		installation
		Account struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"account"`
		SuspendedAt *time.Time `json:"suspended_at"`
	}](ctx, c, jwt, "/app/installations")
	if err != nil {
		return prev, fmt.Errorf("github: the %s App's installations, so the last list of them is used: %w", a.role, err)
	}
	var out []installed
	var errs []error
	for _, in := range installs {
		// GitHub still lists a suspended installation but won't mint it a
		// token (403), and one removed since the list is a 404.
		if in.SuspendedAt != nil {
			continue
		}
		it := installed{installation: in.installation, account: in.Account.Login, org: in.Account.Type == "Organization", repos: map[string]installRepo{}}
		repos, err := c.installationRepos(ctx, a, in.installation)
		if errors.Is(err, forge.ErrNotFound) {
			continue
		}
		if err != nil {
			i := slices.IndexFunc(prev, func(p installed) bool { return p.ID == in.ID })
			if i >= 0 {
				it.repos = prev[i].repos
			}
			errs = append(errs, fmt.Errorf("installation %d, so the %d repos last listed are used: %w", in.ID, len(it.repos), err))
			out = append(out, it)
			continue
		}
		for _, rp := range repos {
			owner, name, ok := strings.Cut(rp.FullName, "/")
			r := c.ref(owner, name)
			if !ok || (c.cfg.Allowed != nil && !c.cfg.Allowed(r)) {
				continue
			}
			it.repos[rp.FullName] = rp
		}
		out = append(out, it)
	}
	// What each repo's installation is saves a lookup when devbot acts there.
	a.mu.Lock()
	for _, it := range out {
		for full := range it.repos {
			a.installs[full] = it.installation
		}
	}
	a.mu.Unlock()
	return out, errors.Join(errs...)
}

// installationRepos is every repo installation in covers: a repo left off
// it would never be reconciled.
func (c *Client) installationRepos(ctx context.Context, a *app, in installation) ([]installRepo, error) {
	tok, _, err := c.installToken(ctx, a, in, "")
	if err != nil {
		return nil, err
	}
	cr := cred{bucket: "installation " + strconv.FormatInt(in.ID, 10), token: tok}
	return listIn(ctx, c, cr, "/installation/repositories", func(page struct {
		Repositories []installRepo `json:"repositories"`
	}) []installRepo {
		return page.Repositories
	})
}

// hasIssues reports whether any of in's repos has issues on: a search in
// an installation without would find nothing devbot can work on.
func (in installed) hasIssues() bool {
	for _, rp := range in.repos {
		if rp.HasIssues {
			return true
		}
	}
	return false
}
