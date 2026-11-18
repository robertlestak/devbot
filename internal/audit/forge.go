package audit

import (
	"context"
	"errors"
	"strings"

	"git.shdw.tech/rob/devbot/internal/forge"
)

// Forge wraps p so that every write it makes as one of the bots is audited,
// whoever in devbot asked for it. Reads pass through. A person's writes are
// audited by whoever holds their token (User).
func Forge(p forge.Provider, l *Logger) forge.Provider {
	if l == nil {
		return p
	}
	return &provider{Provider: p, l: l}
}

// Registry is Forge for every provider in r.
func Registry(r forge.Registry, l *Logger) forge.Registry {
	out := forge.Registry{}
	for n, p := range r {
		out[n] = Forge(p, l)
	}
	return out
}

type provider struct {
	forge.Provider
	l *Logger
}

func (p *provider) bot(role forge.Role) string {
	impl, rev := p.BotUsers()
	if role == forge.RoleReviewer {
		return rev
	}
	return impl
}

// outcome is how a write the forge turned down reads in the record.
func outcome(err error) string {
	switch {
	case err == nil:
		return OK
	case errors.Is(err, forge.ErrForbidden), errors.Is(err, forge.ErrNotMergeable), errors.Is(err, forge.ErrHeadMoved):
		return Refused
	}
	return Error
}

func (p *provider) write(ctx context.Context, role forge.Role, event string, f Fields, detail map[string]any, err error) {
	p.l.Log(ctx, Record{Event: event, Outcome: outcome(err), Actor: p.bot(role), ActorKind: string(role), Err: err, Fields: f, Detail: detail})
}

func item(ref forge.ItemRef) Fields { return Fields{Repo: ref.Repo.String(), Task: ref.String()} }

func pr(repo forge.RepoRef, n int) Fields { return Fields{Repo: repo.String(), PR: n} }

func (p *provider) CreateItem(ctx context.Context, repo forge.RepoRef, title, body string, labels []string) (forge.Item, error) {
	it, err := p.Provider.CreateItem(ctx, repo, title, body, labels)
	f := Fields{Repo: repo.String()}
	if err == nil {
		f.Task = it.Ref.String()
	}
	p.write(ctx, forge.RoleImplementer, "forge.issue.create", f, map[string]any{"title": title, "labels": labels}, err)
	return it, err
}

func (p *provider) CloseItem(ctx context.Context, ref forge.ItemRef) error {
	err := p.Provider.CloseItem(ctx, ref)
	p.write(ctx, forge.RoleImplementer, "forge.issue.close", item(ref), nil, err)
	return err
}

func (p *provider) EditItem(ctx context.Context, ref forge.ItemRef, title, body string) error {
	err := p.Provider.EditItem(ctx, ref, title, body)
	p.write(ctx, forge.RoleImplementer, "forge.issue.edit", item(ref), map[string]any{"title": title}, err)
	return err
}

func (p *provider) Comment(ctx context.Context, ref forge.ItemRef, body string) error {
	err := p.Provider.Comment(ctx, ref, body)
	p.write(ctx, forge.RoleImplementer, "forge.comment.create", item(ref), commentDetail(body), err)
	return err
}

func (p *provider) UpdateComment(ctx context.Context, ref forge.ItemRef, id int64, body string) error {
	err := p.Provider.UpdateComment(ctx, ref, id, body)
	p.write(ctx, forge.RoleImplementer, "forge.comment.update", item(ref), commentDetail(body), err)
	return err
}

func (p *provider) SetLabels(ctx context.Context, ref forge.ItemRef, add, remove []string) error {
	err := p.Provider.SetLabels(ctx, ref, add, remove)
	p.write(ctx, forge.RoleImplementer, "forge.labels.set", item(ref), map[string]any{"add": add, "remove": remove}, err)
	return err
}

func (p *provider) EnsureLabels(ctx context.Context, repo forge.RepoRef, labels []forge.Label) error {
	err := p.Provider.EnsureLabels(ctx, repo, labels)
	names := make([]string, len(labels))
	for i, l := range labels {
		names[i] = l.Name
	}
	p.write(ctx, forge.RoleImplementer, "forge.labels.ensure", Fields{Repo: repo.String()}, map[string]any{"labels": names}, err)
	return err
}

func (p *provider) OpenPR(ctx context.Context, repo forge.RepoRef, head, base, title, body string) (forge.PR, error) {
	got, err := p.Provider.OpenPR(ctx, repo, head, base, title, body)
	p.write(ctx, forge.RoleImplementer, "forge.pr.open", pr(repo, got.Number), map[string]any{"head": head, "base": base, "title": title}, err)
	return got, err
}

func (p *provider) UpdatePR(ctx context.Context, repo forge.RepoRef, number int, title, body string) error {
	err := p.Provider.UpdatePR(ctx, repo, number, title, body)
	p.write(ctx, forge.RoleImplementer, "forge.pr.update", pr(repo, number), map[string]any{"title": title}, err)
	return err
}

func (p *provider) SetPRBase(ctx context.Context, repo forge.RepoRef, number int, base string) error {
	err := p.Provider.SetPRBase(ctx, repo, number, base)
	p.write(ctx, forge.RoleImplementer, "forge.pr.retarget", pr(repo, number), map[string]any{"base": base}, err)
	return err
}

func (p *provider) RerunChecks(ctx context.Context, repo forge.RepoRef, sha string, checks []forge.CheckDetail) error {
	err := p.Provider.RerunChecks(ctx, repo, sha, checks)
	names := make([]string, 0, len(checks))
	for _, c := range checks {
		names = append(names, c.Context)
	}
	p.write(ctx, forge.RoleImplementer, "forge.checks.rerun", Fields{Repo: repo.String()}, map[string]any{"sha": sha, "checks": names}, err)
	return err
}

func (p *provider) SetCommitStatus(ctx context.Context, repo forge.RepoRef, sha string, s forge.CommitStatus) error {
	err := p.Provider.SetCommitStatus(ctx, repo, sha, s)
	p.write(ctx, forge.RoleImplementer, "forge.status.set", Fields{Repo: repo.String()}, map[string]any{"sha": sha, "context": s.Context, "state": s.State}, err)
	return err
}

func (p *provider) Review(ctx context.Context, repo forge.RepoRef, number int, sha string, v forge.Verdict, body string) error {
	err := p.Provider.Review(ctx, repo, number, sha, v, body)
	p.write(ctx, forge.RoleReviewer, "forge.pr.review", pr(repo, number), map[string]any{"verdict": string(v), "sha": sha}, err)
	return err
}

func (p *provider) Merge(ctx context.Context, repo forge.RepoRef, number int, sha, method string) error {
	err := p.Provider.Merge(ctx, repo, number, sha, method)
	p.write(ctx, forge.RoleReviewer, "forge.pr.merge", pr(repo, number), map[string]any{"sha": sha, "method": method}, err)
	return err
}

func (p *provider) AttachToPR(ctx context.Context, repo forge.RepoRef, number int, name string, data []byte) (string, error) {
	url, err := p.Provider.AttachToPR(ctx, repo, number, name, data)
	p.write(ctx, forge.RoleImplementer, "forge.attachment.upload", pr(repo, number), map[string]any{"name": name, "bytes": len(data)}, err)
	return url, err
}

func (p *provider) ReleaseAttachments(ctx context.Context, repo forge.RepoRef, number int) error {
	err := p.Provider.ReleaseAttachments(ctx, repo, number)
	p.write(ctx, forge.RoleImplementer, "forge.attachment.release", pr(repo, number), nil, err)
	return err
}

// DeleteBranch records a branch the forge had already deleted as ok: devbot
// asked, and the branch is gone, which is all it wanted (see forge.go).
func (p *provider) DeleteBranch(ctx context.Context, repo forge.RepoRef, name string) error {
	err := p.Provider.DeleteBranch(ctx, repo, name)
	d := map[string]any{"branch": name}
	rerr := err
	if errors.Is(err, forge.ErrNotFound) {
		d["already_gone"], rerr = true, nil
	}
	p.write(ctx, forge.RoleImplementer, "forge.branch.delete", Fields{Repo: repo.String()}, d, rerr)
	return err
}

// commentDetail says what a comment was without copying it: bodies are
// people's and the model's text, and can be long. A devbot status marker
// says what the comment did.
func commentDetail(body string) map[string]any {
	d := map[string]any{"bytes": len(body)}
	if i := strings.Index(body, "<!-- devbot:"); i >= 0 {
		kind, _, _ := strings.Cut(strings.TrimSpace(body[i+len("<!-- devbot:"):]), " ")
		d["marker"] = kind
	}
	return d
}

// User wraps u, the forge as the person signed in as login on provider, so
// that each write made with their token is audited as theirs.
func User(u forge.User, l *Logger, login string) forge.User {
	if l == nil {
		return u
	}
	return &user{User: u, l: l, login: login}
}

type user struct {
	forge.User
	l     *Logger
	login string
}

func (u *user) write(ctx context.Context, event string, f Fields, detail map[string]any, err error) {
	u.l.Log(ctx, Record{Event: event, Outcome: outcome(err), Actor: u.login, ActorKind: "user", Err: err, Fields: f, Detail: detail})
}

func (u *user) CreateItem(ctx context.Context, repo forge.RepoRef, title, body string, labels []string) (forge.Item, error) {
	it, err := u.User.CreateItem(ctx, repo, title, body, labels)
	f := Fields{Repo: repo.String()}
	if err == nil {
		f.Task = it.Ref.String()
	}
	u.write(ctx, "forge.issue.create", f, map[string]any{"title": title, "labels": labels}, err)
	return it, err
}

func (u *user) CloseItem(ctx context.Context, ref forge.ItemRef) error {
	err := u.User.CloseItem(ctx, ref)
	u.write(ctx, "forge.issue.close", item(ref), nil, err)
	return err
}

func (u *user) Comment(ctx context.Context, ref forge.ItemRef, body string) error {
	err := u.User.Comment(ctx, ref, body)
	u.write(ctx, "forge.comment.create", item(ref), commentDetail(body), err)
	return err
}

func (u *user) SetLabels(ctx context.Context, ref forge.ItemRef, add, remove []string) error {
	err := u.User.SetLabels(ctx, ref, add, remove)
	u.write(ctx, "forge.labels.set", item(ref), map[string]any{"add": add, "remove": remove}, err)
	return err
}
