package core

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"git.shdw.tech/rob/devbot/internal/changeset"
	"git.shdw.tech/rob/devbot/internal/forge"
)

// changesMax caps the remembered change sets. They are only an optimisation:
// an approved PR waiting for a person is gated again on every evaluation.
const changesMax = 1000

// changeCache remembers change sets by PR head and base branch, which is
// all one depends on short of the base being rewritten.
type changeCache struct {
	mu sync.Mutex
	m  map[string][]changeset.Change
}

// changeSet lists what pr changes against its base, from git, fetched as the
// reviewer.
func (e *Engine) changeSet(ctx context.Context, p forge.Provider, pr forge.PR) ([]changeset.Change, error) {
	key := pr.Repo.String() + "\x00" + pr.BaseBranch + "\x00" + pr.HeadBranch + "\x00" + pr.HeadSHA
	e.changes.mu.Lock()
	cs, ok := e.changes.m[key]
	e.changes.mu.Unlock()
	if ok {
		return cs, nil
	}
	fetch := e.Changes
	if fetch == nil {
		fetch = e.fetchChanges
	}
	cs, err := fetch(ctx, p, pr)
	if err != nil {
		return nil, err
	}
	e.changes.mu.Lock()
	if e.changes.m == nil {
		e.changes.m = map[string][]changeset.Change{}
	}
	makeRoom(e.changes.m, changesMax)
	e.changes.m[key] = cs
	e.changes.mu.Unlock()
	return cs, nil
}

// fetchChanges fetches them as the reviewer.
func (e *Engine) fetchChanges(ctx context.Context, p forge.Provider, pr forge.PR) ([]changeset.Change, error) {
	return e.fetchChangesAs(ctx, p, pr, forge.RoleReviewer)
}

// fetchChangesAs asks for role's git credential on every fetch, which a
// GitHub installation token, lasting an hour, needs.
func (e *Engine) fetchChangesAs(ctx context.Context, p forge.Provider, pr forge.PR, role forge.Role) ([]changeset.Change, error) {
	url, header, err := p.GitAuth(ctx, pr.Repo, role)
	if err != nil {
		return nil, err
	}
	e.Audit.Secret(header, header[strings.LastIndex(header, " ")+1:])
	// A fork's branch isn't the repo's, but the PR's head is.
	head := "refs/heads/" + pr.HeadBranch
	if pr.Fork {
		head = fmt.Sprintf("refs/pull/%d/head", pr.Number)
	}
	return changeset.Fetch(ctx, os.TempDir(), url, header, pr.BaseBranch, head, pr.HeadSHA)
}
