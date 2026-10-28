package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"git.shdw.tech/rob/devbot/internal/forge"
)

// actions are the actions of each event that can move a task, by
// X-GitHub-Event. nil takes every action; an event that isn't here is
// ignored. pull_request_review_comment isn't here: a line comment on a
// diff is nothing core reads, and a review's own comments come with its
// pull_request_review.
var actions = map[string][]string{
	"issues":                    {"opened", "edited", "closed", "reopened", "labeled", "unlabeled"},
	"issue_comment":             nil,
	"pull_request":              {"opened", "synchronize", "edited", "closed", "reopened", "ready_for_review", "labeled"},
	"pull_request_review":       nil,
	"check_run":                 nil,
	"check_suite":               nil,
	"status":                    nil,
	"push":                      nil,
	"installation":              nil,
	"installation_repositories": nil,
}

type hookRepo struct {
	ID            int64  `json:"id"`
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
}

// checkPRs is a check run's or suite's pull_requests: the PRs whose head
// it ran on.
type checkPRs struct {
	HeadSHA      string `json:"head_sha"`
	PullRequests []struct {
		Number int     `json:"number"`
		Head   hookRef `json:"head"`
		Base   struct {
			Repo struct {
				ID int64 `json:"id"`
			} `json:"repo"`
		} `json:"base"`
	} `json:"pull_requests"`
}

// ParseWebhook verifies X-Hub-Signature-256 ("sha256=" and the hex
// HMAC-SHA256 of the body) and maps the delivery onto the objects it is
// about, as the Gitea adapter does (docs: "Validating webhook
// deliveries", "Webhook events and payloads"). Past identifiers,
// it reads only what lets the engine skip a delivery that can't concern
// devbot (forge.Event.Facts): the engine re-reads everything else, because
// deliveries are retried and arrive out of order.
//
// Only the implementer App's webhook should be enabled. A delivery from
// any other hook (the reviewer App's, left on by mistake, would bring
// every event a second time under new delivery IDs) is ignored.
func (c *Client) ParseWebhook(r *http.Request, body []byte) ([]forge.Event, error) {
	if c.cfg.WebhookSecret == "" {
		return nil, errors.New("github: no webhook secret configured")
	}
	hexSig, ok := strings.CutPrefix(r.Header.Get("X-Hub-Signature-256"), "sha256=")
	sig, err := hex.DecodeString(hexSig)
	if !ok || err != nil || len(sig) == 0 {
		return nil, errors.New("github: missing or malformed signature")
	}
	mac := hmac.New(sha256.New, []byte(c.cfg.WebhookSecret))
	mac.Write(body)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return nil, errors.New("github: bad signature")
	}

	typ := r.Header.Get("X-GitHub-Event")
	ignored := []forge.Event{{DeliveryID: r.Header.Get("X-GitHub-Delivery"), Kind: forge.EventIgnored}}
	// An App's webhook is the "integration" target, its ID the App's.
	if id := r.Header.Get("X-GitHub-Hook-Installation-Target-ID"); id != "" &&
		(id != strconv.FormatInt(c.cfg.ImplementerAppID, 10) || r.Header.Get("X-GitHub-Hook-Installation-Target-Type") != "integration") {
		return ignored, nil
	}
	acts, known := actions[typ]
	if !known {
		return ignored, nil
	}

	var p struct {
		Action      string    `json:"action"`
		Repository  *hookRepo `json:"repository"`
		Sender      user      `json:"sender"`
		Issue       *issue    `json:"issue"`
		PullRequest *struct {
			Number int     `json:"number"`
			Labels []label `json:"labels"`
			Head   hookRef `json:"head"`
		} `json:"pull_request"`
		Comment *struct {
			Body string `json:"body"`
		} `json:"comment"`
		CheckRun   *checkPRs `json:"check_run"`
		CheckSuite *checkPRs `json:"check_suite"`
		SHA        string    `json:"sha"`
		Ref        string    `json:"ref"`
		Commits    []struct {
			Added    []string `json:"added"`
			Removed  []string `json:"removed"`
			Modified []string `json:"modified"`
		} `json:"commits"`
		Installation *struct {
			ID    int64 `json:"id"`
			AppID int64 `json:"app_id"`
		} `json:"installation"`
		Repositories        []hookRepo `json:"repositories"`
		RepositoriesAdded   []hookRepo `json:"repositories_added"`
		RepositoriesRemoved []hookRepo `json:"repositories_removed"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("github: decode webhook: %w", err)
	}
	ignored[0].Actor = p.Sender.Login
	if acts != nil && !slices.Contains(acts, p.Action) {
		return ignored, nil
	}
	if typ == "installation" || typ == "installation_repositories" {
		if p.Installation == nil || p.Installation.AppID != c.cfg.ImplementerAppID {
			return ignored, nil
		}
		return c.installationEvents(r.Context(), ignored[0], p.Installation.ID, p.Action, slices.Concat(p.Repositories, p.RepositoriesAdded, p.RepositoriesRemoved)), nil
	}
	if p.Repository == nil {
		return ignored, nil
	}
	owner, name, _ := strings.Cut(p.Repository.FullName, "/")
	ev := ignored[0]
	ev.Repo = c.ref(owner, name)

	switch typ {
	case "push":
		// A tag pushed, or made with a release, comes as a push too.
		if strings.HasPrefix(p.Ref, "refs/tags/") {
			ev.Kind = forge.EventTag
			break
		}
		if p.Ref == "refs/heads/"+p.Repository.DefaultBranch {
			ev.Kind = forge.EventDefaultBranch
			for _, cm := range p.Commits {
				ev.Paths = append(ev.Paths, cm.Added...)
				ev.Paths = append(ev.Paths, cm.Removed...)
				ev.Paths = append(ev.Paths, cm.Modified...)
			}
		}
	case "status":
		ev.Kind, ev.SHA = forge.EventStatus, p.SHA
	case "check_run", "check_suite":
		ck := p.CheckRun
		if typ == "check_suite" {
			ck = p.CheckSuite
		}
		if ck == nil {
			break
		}
		// pull_requests names the PRs straight away, but GitHub leaves it
		// empty for a head on a fork; the head SHA still finds those.
		var prs []forge.Event
		for _, pr := range ck.PullRequests {
			if pr.Base.Repo.ID == p.Repository.ID {
				e := ev
				e.Kind, e.Number = forge.EventPR, pr.Number
				e.Facts, e.Head = true, pr.Head.Ref
				prs = append(prs, e)
			}
		}
		if len(prs) > 0 {
			return distinctIDs(prs), nil
		}
		ev.Kind, ev.SHA = forge.EventStatus, ck.HeadSHA
	case "pull_request", "pull_request_review":
		if p.PullRequest != nil {
			ev.Kind, ev.Number = forge.EventPR, p.PullRequest.Number
			ev.Facts, ev.Labels, ev.Head = true, names(p.PullRequest.Labels), p.PullRequest.Head.Ref
		}
	case "issues", "issue_comment":
		// A PR's conversation is an issue's on GitHub, so a comment on a
		// PR comes as issue_comment, with issue.pull_request set.
		switch {
		case p.Issue == nil:
		case p.Issue.PullRequest != nil:
			ev.Kind, ev.Number = forge.EventPR, p.Issue.Number
			ev.Facts, ev.Labels = true, names(p.Issue.Labels)
		default:
			ev.Kind, ev.Number = forge.EventItem, p.Issue.Number
			ev.Facts, ev.Labels = true, names(p.Issue.Labels)
		}
		if p.Comment != nil {
			ev.Comment = p.Comment.Body
		}
	}
	return []forge.Event{ev}, nil
}

// installationEvents handles the implementer App gaining or losing repos.
// The adapter forgets what it knew of their installation, and each repo
// gets a default-branch event, so the engine re-reads whether devbot
// works there instead of trusting a cached answer from before. The repo
// inventory reconcile searches in is listed again. A bump of the shared
// generation that fails isn't the delivery's fault, so it isn't refused
// for it: inventoryTTL bounds how long the reconciler goes on with the old
// list.
func (c *Client) installationEvents(ctx context.Context, ev forge.Event, id int64, action string, repos []hookRepo) []forge.Event {
	_ = c.invalidateInventory(ctx)
	a := c.impl
	a.mu.Lock()
	for _, r := range repos {
		delete(a.installs, r.FullName)
	}
	if action == "deleted" || action == "suspend" {
		for k := range a.tokens {
			if k.install == id {
				delete(a.tokens, k)
			}
		}
	}
	a.mu.Unlock()
	var evs []forge.Event
	for _, r := range repos {
		owner, name, ok := strings.Cut(r.FullName, "/")
		if !ok {
			continue
		}
		e := ev
		e.Kind, e.Repo = forge.EventDefaultBranch, c.ref(owner, name)
		evs = append(evs, e)
	}
	if len(evs) == 0 {
		return []forge.Event{ev}
	}
	return distinctIDs(evs)
}

// distinctIDs gives each of one delivery's events an ID of its own, as
// serve dedupes on it, and the same ID each time GitHub redelivers.
func distinctIDs(evs []forge.Event) []forge.Event {
	if len(evs) < 2 || evs[0].DeliveryID == "" {
		return evs
	}
	for i := range evs {
		evs[i].DeliveryID += "/" + evs[i].Repo.FullName() + "#" + strconv.Itoa(evs[i].Number)
	}
	return evs
}

// hookRef is a PR's head or base as a delivery names it.
type hookRef struct {
	Ref string `json:"ref"`
}

func names(ls []label) []string {
	var out []string
	for _, l := range ls {
		out = append(out, l.Name)
	}
	return out
}
