package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"git.shdw.tech/rob/devbot/internal/forge"
)

// PinOf is what an issue says, as a hash of its title and one of its body,
// so a pin shows which of the two changed.
func PinOf(it forge.Item) string {
	t, b := sha256.Sum256([]byte(it.Title)), sha256.Sum256([]byte(it.Body))
	return hex.EncodeToString(t[:]) + "." + hex.EncodeToString(b[:])
}

// changedFields are the fields that differ between two pins. Against an
// unknown pin nothing is known to have changed.
func changedFields(old, cur string) []string {
	if old == "" {
		return nil
	}
	ot, ob, _ := strings.Cut(old, ".")
	ct, cb, _ := strings.Cut(cur, ".")
	var out []string
	if ot != ct {
		out = append(out, forge.FieldTitle)
	}
	if ob != cb {
		out = append(out, forge.FieldBody)
	}
	return out
}

// Approval is who last approved the issue for devbot and when: the latest
// devbot/ready applied by a person who can direct devbot, or the issue's
// author and creation when nobody did (filed with the label on). The bots
// putting the label back after a failed run approve nothing.
func (e *Engine) Approval(ctx context.Context, it forge.Item) (string, time.Time, error) {
	p, err := e.provider(it.Ref.Repo)
	if err != nil {
		return "", time.Time{}, err
	}
	rc, err := e.Repos.Get(ctx, it.Ref.Repo)
	if err != nil {
		return "", time.Time{}, err
	}
	evs, err := p.LabelEvents(ctx, it.Ref)
	if err != nil {
		return "", time.Time{}, err
	}
	bots := e.bots(p)
	for i := len(evs) - 1; i >= 0; i-- {
		ev := evs[i]
		if ev.Label == LReady && ev.Added && !forge.HasLogin(bots, ev.Actor) && e.authorized(ctx, p, rc, it.Ref.Repo, ev.Actor) {
			return ev.Actor, ev.At, nil
		}
	}
	return it.Author, it.CreatedAt, nil
}

// CheckPin holds the issue to what devbot was approved to work from, before
// an implement, fix or review job uses it. The issue is pinned by the latest
// "started" or "pinned" status since the last retry; before the first, by
// accepted (the job's own pin, from when devbot accepted the label) and the
// approval itself.
//
// An issue that changed since then carries on only if every edit since was
// made by someone who can direct devbot, and every field that changed has an
// edit to show for it; when devbot was already pinned, the new text is
// pinned with a comment saying who changed it. Otherwise the task stops, and
// CheckPin reports false: someone who can't direct devbot, or someone the
// forge doesn't name, would be writing its instructions.
//
// It returns the pin to record in the job's "started" status.
func (e *Engine) CheckPin(ctx context.Context, it forge.Item, comments []forge.Comment, accepted string) (string, bool, error) {
	p, err := e.provider(it.Ref.Repo)
	if err != nil {
		return "", false, err
	}
	rc, err := e.Repos.Get(ctx, it.Ref.Repo)
	if err != nil {
		return "", false, err
	}
	cur := PinOf(it)
	ss := statuses(comments, e.bots(p)...)
	var pinned *statusComment
	for i := len(ss) - 1; i >= 0 && ss[i].Kind != "reset"; i-- {
		if ss[i].Pin != "" {
			pinned = &ss[i]
			break
		}
	}
	old, since := accepted, time.Time{}
	if pinned != nil {
		if pinned.Pin == cur {
			return cur, true, nil
		}
		old, since = pinned.Pin, pinned.CreatedAt
	} else if _, since, err = e.Approval(ctx, it); err != nil {
		return "", false, err
	}
	edits, err := p.ContentEdits(ctx, it.Ref)
	if err != nil {
		return "", false, err
	}
	// Forges keep some times to the second: an edit in the same second
	// counts as after, which can only stop a task, never let one through.
	since = since.Truncate(time.Second)
	edited := map[string]bool{}
	var editors, strangers []string
	unknown := false
	for _, ed := range edits {
		if ed.At.Before(since) {
			continue
		}
		edited[ed.Field] = true
		switch {
		case ed.Editor == "":
			unknown = true
		case forge.HasLogin(editors, ed.Editor) || forge.HasLogin(strangers, ed.Editor):
		case e.authorized(ctx, p, rc, it.Ref.Repo, ed.Editor):
			editors = append(editors, ed.Editor)
		default:
			strangers = append(strangers, ed.Editor)
		}
	}
	for _, f := range changedFields(old, cur) {
		unknown = unknown || !edited[f]
	}
	if len(strangers) > 0 || unknown {
		why := "The issue's title or body changed after it was approved for devbot"
		if len(strangers) > 0 {
			why += fmt.Sprintf(", edited by %s, who can't direct devbot", mentions(strangers))
		} else {
			why += ", and the forge doesn't say who changed it"
		}
		why += fmt.Sprintf(". devbot only works from text someone who can direct it has approved. A maintainer should read the issue as it stands and, if it's right, re-apply `%s`.", LReady)
		return "", false, e.NeedsHuman(ctx, it, why)
	}
	if pinned != nil {
		body := fmt.Sprintf("The issue changed after it was approved for devbot. %s, who can direct devbot, made the change, so devbot carries on from the issue as it stands.\n\n%s",
			mentions(editors), RenderStatus(Status{Kind: "pinned", Pin: cur}))
		if err := p.Comment(ctx, it.Ref, body); err != nil {
			return "", false, err
		}
	}
	return cur, true, nil
}

func mentions(users []string) string {
	return "@" + strings.Join(users, ", @")
}
