package harness

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"git.shdw.tech/rob/devbot/internal/audit"
	"git.shdw.tech/rob/devbot/internal/changeset"
	"git.shdw.tech/rob/devbot/internal/config"
	"git.shdw.tech/rob/devbot/internal/core"
	"git.shdw.tech/rob/devbot/internal/forge"
)

// Work that changes protected files a person may approve (approvable_paths)
// is held: kept on the forge at core.HeldRef, which no CI runs on, and shown
// in full on the issue, until someone who can direct devbot approves it.
// A held job then pushes exactly that commit (releaseHeld).

// heldError is work that changes protected files a person may approve that
// no approval on the task covers. push returns it instead of pushing.
type heldError struct {
	Branch  string
	Changes []changeset.Change
}

func (e *heldError) Error() string {
	paths := make([]string, len(e.Changes))
	for i, c := range e.Changes {
		paths[i] = c.Path
	}
	return "the work changes protected files that need a person's approval, so it wasn't pushed to " + e.Branch + ":\n" + strings.Join(paths, "\n")
}

// approvedIn is whether an approval among comments' statuses covers a
// change's protected files, for push.
func approvedIn(comments []forge.Comment, bots []string) func([]core.HeldFile) bool {
	ss := core.Statuses(comments, bots...)
	return func(files []core.HeldFile) bool { return core.Approved(ss, files) }
}

// maxHoldComment keeps a hold's comment under what the forges take in one
// (GitHub's is 65,536 characters).
const maxHoldComment = 60000

// heldPRRef is where the private clone keeps a held change's PR text.
const heldPRRef = "refs/devbot/held-pr"

// hold keeps the collected work on the forge without pushing it to the
// branch, shows a person what its protected files will hold, and parks the
// task at needs-approval.
func (r *Runner) hold(ctx context.Context, pb *pub, wk work, he *heldError) (string, error) {
	w, n := pb.w, pb.it.Ref.Number
	head, err := w.git(ctx, false, "rev-parse", "refs/devbot/work")
	if err != nil {
		return "", err
	}
	// What a person can't read in the diff, they can't approve.
	bad, err := w.unreadable(ctx, he.Changes)
	if err != nil {
		return "", err
	}
	if len(bad) > 0 {
		return "protected", r.needsHuman(ctx, pb.settle, fmt.Sprintf("The implementer's work changes protected files that a person may approve, but what it leaves in these isn't text a person can read in a diff (binary, or not UTF-8), so devbot didn't push it to `%s` or hold it for approval:\n\n- %s\n\n"+
			"A person has to make changes like these. If the task can be done without them, say how in the issue and add `%s` to retry.",
			pb.branch, strings.Join(bad, "\n- "), core.LReady))
	}
	auto := pb.rc.Approval == config.ApprovalAutoClean && !pb.autoApproved
	text, self, err := w.holdText(ctx, pb, wk, he.Changes, head, auto)
	if err != nil {
		return "", fmt.Errorf("showing the held change: %w", err)
	}
	pr := 0
	if pb.pr != nil {
		pr = pb.pr.Number
	}
	files := core.HeldFiles(he.Changes)
	st := core.Status{Kind: "held", SHA: head, PR: pr, Base: pb.base, Branch: pb.branch, Files: files}
	if self {
		st.Mode = "auto"
	}
	body := text + "\n\n" + core.RenderStatus(st)
	if len(body) > maxHoldComment {
		var b strings.Builder
		for _, f := range files {
			b.WriteString("\n- " + config.ShowPath(f.Path))
		}
		return "protected", r.needsHuman(ctx, pb.settle, fmt.Sprintf("The implementer's work changes protected files that a person may approve, but the change is too big to show in a comment (%s), so devbot didn't push it to `%s` or hold it for approval:\n%s\n\n"+
			"A person has to make changes like these. If the task can be done with a smaller change to them, say how in the issue and add `%s` to retry.",
			size(int64(len(body))), pb.branch, b.String(), core.LReady))
	}
	if self {
		return r.autoApprove(ctx, pb, wk, st, body)
	}
	meta, err := w.heldPR(ctx, wk)
	if err != nil {
		return "", fmt.Errorf("keeping the held change's PR text: %w", err)
	}
	if err := w.pushHeld(ctx, n, head, meta); err != nil {
		var refused *pushRefusedError
		if errors.As(err, &refused) {
			return "refused", r.needsHuman(ctx, pb.settle, pushRefusedText(refused))
		}
		return "", err
	}
	return "held", pb.settle(ctx, func(it forge.Item) error {
		if err := pb.p.Comment(ctx, it.Ref, body); err != nil {
			return err
		}
		return r.E.Transition(ctx, it, core.LNeedsApproval, "task.needs_approval",
			fmt.Sprintf("holding a change to %d protected file(s) for a person's approval", len(files)))
	})
}

// autoApprove pushes a held change whose risk read flagged nothing, under
// approval: auto_clean, from the run that made it: it shows the change as a
// hold does (body), records devbot's own approval of it, then publishes it,
// so the merge gate and later pushes hold its protected files to that
// content as they do after a person's approval. Nothing waits, so nothing
// is kept at the held refs.
func (r *Runner) autoApprove(ctx context.Context, pb *pub, wk work, hold core.Status, body string) (string, error) {
	ref := pb.it.Ref
	if err := pb.p.Comment(ctx, ref, body); err != nil {
		return "", err
	}
	if err := pb.p.Comment(ctx, ref, core.AutoApprovalText(hold)); err != nil {
		return "", err
	}
	r.E.Count(ctx, "held_approvals", []string{ref.Repo.String(), "auto"}, 1)
	pb.log.Info("pushing a held change its clean risk read approved", "sha", hold.SHA)
	// publish reads the approval back; should it not see it, the push is
	// held again, for a person this time.
	pb.autoApproved = true
	// Already attributed: the commit pushed is the one shown.
	pb.w.held = true
	return r.publish(ctx, pb, wk)
}

// releaseHeld is a held job: it pushes the change a person approved, as
// devbot showed it, and opens or updates its PR with the text the run that
// held it left. A change that is no longer the one shown is held again,
// with a new comment, instead.
func (r *Runner) releaseHeld(ctx context.Context, pb *pub, back string) (string, error) {
	w, n := pb.w, pb.it.Ref.Number
	ss := core.Statuses(pb.comments, r.E.Bots(pb.it.Ref.Repo)...)
	hold, _ := core.LastHold(ss)
	approval, ok := core.ApprovalOf(ss, hold)
	if !ok {
		return "held", pb.settle(ctx, func(it forge.Item) error { return r.E.Transition(ctx, it, back, "", "") })
	}
	if out, err := w.git(ctx, true, "fetch", "--quiet", "--no-tags", "origin",
		"+"+core.HeldRef(n)+":refs/devbot/work", "+"+core.HeldPRRef(n)+":"+heldPRRef); err != nil {
		if strings.Contains(out, "couldn't find remote ref") {
			return "held-gone", r.needsHuman(ctx, pb.settle, fmt.Sprintf("The change approved on this issue is no longer at `%s` on the forge, so devbot has nothing to push. Add `%s` to start the task over.", core.HeldRef(n), core.LReady))
		}
		return "", fmt.Errorf("fetching the held change: %w", err)
	}
	w.held = true
	wk, err := w.readHeldPR(ctx)
	if err != nil {
		return "", fmt.Errorf("reading the held change's PR text: %w", err)
	}
	head, err := w.git(ctx, false, "rev-parse", "refs/devbot/work")
	if err != nil {
		return "", err
	}
	if head != approval.SHA {
		// Replaced since it was approved: devbot pushes only what it
		// showed, and nobody approved this.
		return "held-replaced", r.needsHuman(ctx, pb.settle, fmt.Sprintf("The change approved on this issue was `%s`, but `%s` on the forge now holds `%s`, which nobody approved, so devbot pushed nothing. "+
			"Look at who changed the ref, then add `%s` to start the task over.", approval.SHA, core.HeldRef(n), head, core.LReady))
	}
	outcome, err := r.publish(ctx, pb, wk)
	if err == nil && outcome == "pr" {
		w.dropHeld(ctx, n)
	}
	return outcome, err
}

// pushHeld keeps head, the held work, and meta, its PR text, on the forge,
// in refs that are neither branches nor tags: forges run CI on pushes to
// those alone (verified on Gitea; GitHub documents it). Both or neither.
func (w *workspace) pushHeld(ctx context.Context, n int, head, meta string) error {
	out, err := w.git(ctx, true, "push", "--quiet", "--force", "--atomic", "origin", head+":"+core.HeldRef(n), meta+":"+core.HeldPRRef(n))
	w.audit.Log(ctx, audit.Record{Event: "git.push", Outcome: pushOutcome(out, err), Actor: w.actor, ActorKind: string(w.role), Err: err,
		Detail: map[string]any{"ref": core.HeldRef(n), "sha": head, "force": true, "held": true}})
	if err != nil {
		if e := w.refusal(ctx, core.HeldRef(n), out); e != nil {
			return e
		}
	}
	return err
}

// dropHeld deletes a held change's refs once it is on its branch. Best
// effort: a later hold of the task overwrites them anyway.
func (w *workspace) dropHeld(ctx context.Context, n int) {
	out, err := w.git(ctx, true, "push", "--quiet", "origin", ":"+core.HeldRef(n), ":"+core.HeldPRRef(n))
	w.audit.Log(ctx, audit.Record{Event: "git.push", Outcome: pushOutcome(out, err), Actor: w.actor, ActorKind: string(w.role), Err: err,
		Detail: map[string]any{"ref": core.HeldRef(n), "delete": true, "held": true}})
}

// heldPRFile is one attachment of a held change, as heldPR keeps it.
type heldPRFile struct {
	Name    string `json:"name"`
	Caption string `json:"caption"`
	Type    string `json:"type"`
	SHA256  string `json:"sha256"`
	File    string `json:"file"`
}

type heldPRNote struct {
	Name string `json:"name"`
	Why  string `json:"why"`
}

type heldPRIndex struct {
	Files []heldPRFile `json:"files"`
	Notes []heldPRNote `json:"notes"`
}

// heldPR writes what the PR of held work is opened with into a commit of
// its own, in the private clone: the model's result, the setup script, the
// checks' output and the attachments, none of which outlive the pod
// otherwise. It returns the commit.
func (w *workspace) heldPR(ctx context.Context, wk work) (string, error) {
	result, err := json.Marshal(wk.out)
	if err != nil {
		return "", err
	}
	idx := heldPRIndex{Files: []heldPRFile{}, Notes: []heldPRNote{}}
	entries := map[string][]byte{"result.json": result, "setup.sh": wk.setup, "checks.txt": []byte(wk.output)}
	for i, f := range wk.files {
		name := fmt.Sprintf("attachment-%d", i)
		idx.Files = append(idx.Files, heldPRFile{Name: f.name, Caption: f.caption, Type: f.typ, SHA256: f.sum, File: name})
		entries[name] = f.data
	}
	for _, nt := range wk.notes {
		idx.Notes = append(idx.Notes, heldPRNote{Name: nt.name, Why: nt.why})
	}
	if entries["attachments.json"], err = json.Marshal(idx); err != nil {
		return "", err
	}
	var tree strings.Builder
	for name, data := range entries {
		sha, err := runGitIO(ctx, w.priv, "", false, nil, string(data), "hash-object", "-w", "--stdin")
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&tree, "100644 blob %s\t%s\n", sha, name)
	}
	t, err := runGitIO(ctx, w.priv, "", false, nil, tree.String(), "mktree")
	if err != nil {
		return "", err
	}
	env := []string{"GIT_AUTHOR_NAME=" + w.name, "GIT_AUTHOR_EMAIL=" + w.email, "GIT_COMMITTER_NAME=" + w.name, "GIT_COMMITTER_EMAIL=" + w.email}
	return runGitIO(ctx, w.priv, "", false, env, "", "commit-tree", t, "-m", "devbot: the PR text of held work")
}

// readHeldPR reads back what heldPR kept, fetched to heldPRRef. It is the
// model's, as it was in the shared volume, and anyone who can push may have
// rewritten it since, so the attachments are checked again as
// readAttachments checks them.
func (w *workspace) readHeldPR(ctx context.Context) (work, error) {
	var wk work
	result, err := w.blob(ctx, heldPRRef+":result.json", 256<<10)
	if err != nil {
		return wk, err
	}
	if err := json.Unmarshal(result, &wk.out); err != nil {
		return wk, fmt.Errorf("result.json: %w", err)
	}
	if wk.setup, err = w.blob(ctx, heldPRRef+":setup.sh", 16<<10); err != nil {
		return wk, err
	}
	output, err := w.blob(ctx, heldPRRef+":checks.txt", 1<<20)
	if err != nil {
		return wk, err
	}
	wk.output = string(output)
	raw, err := w.blob(ctx, heldPRRef+":attachments.json", 64<<10)
	if err != nil {
		return wk, err
	}
	var idx heldPRIndex
	if err := json.Unmarshal(raw, &idx); err != nil {
		return wk, fmt.Errorf("attachments.json: %w", err)
	}
	for _, nt := range idx.Notes {
		wk.notes = append(wk.notes, attachmentNote{oneLine(nt.Name, 80), oneLine(nt.Why, 300)})
	}
	total := 0
	for _, f := range idx.Files {
		data, err := w.blob(ctx, heldPRRef+":"+f.File, maxAttachmentSize+1)
		h := sha256.Sum256(data)
		typ := http.DetectContentType(data)
		if _, ok := attachmentTypes[typ]; err != nil || !ok || typ != f.Type || len(data) > maxAttachmentSize ||
			total+len(data) > maxAttachmentsTotal || len(wk.files) >= maxAttachments || hex.EncodeToString(h[:]) != f.SHA256 {
			wk.notes = append(wk.notes, attachmentNote{oneLine(f.Name, 80), "couldn't be read back from the held change"})
			continue
		}
		total += len(data)
		wk.files = append(wk.files, attachment{name: oneLine(f.Name, 80), caption: oneLine(f.Caption, 300), typ: typ, sum: f.SHA256, data: data})
	}
	return wk, nil
}

// blob reads an object from the private clone as it is, up to max bytes.
func (w *workspace) blob(ctx context.Context, rev string, max int64) ([]byte, error) {
	cmd := gitCmd(ctx, w.priv, "", false, "cat-file", "blob", rev)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &limitWriter{&out, max}, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git cat-file %s: %v: %s", rev, err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

// limitWriter keeps the first n bytes written to it and drops the rest.
type limitWriter struct {
	b *bytes.Buffer
	n int64
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if room := l.n - int64(l.b.Len()); room > 0 {
		l.b.Write(p[:min(int64(len(p)), room)])
	}
	return len(p), nil
}

// holdText shows a person the held change: its protected files' diffs in
// full, devbot's risk read of them, what else it changes, and how to
// approve it. With auto (approval: auto_clean), a change the read finds
// nothing in is shown as approved by devbot instead, and it reports that.
func (w *workspace) holdText(ctx context.Context, pb *pub, wk work, changes []changeset.Change, head string, auto bool) (string, bool, error) {
	base := "refs/remotes/origin/" + w.base
	var files []riskFile
	var diffs strings.Builder
	seen := map[string]bool{}
	for _, c := range changes {
		if seen[c.Path] {
			continue
		}
		seen[c.Path] = true
		patch, err := w.textDiff(ctx, "--no-color", "--no-ext-diff", "--no-textconv", "--no-renames", base+"...refs/devbot/work", "--", c.Path)
		if err != nil {
			return "", false, err
		}
		f := riskFile{Path: c.Path, Patch: patch}
		for _, side := range []struct {
			blob string
			to   *string
		}{{c.OldBlob, &f.Old}, {c.NewBlob, &f.New}} {
			if strings.Trim(side.blob, "0") == "" {
				continue
			}
			b, err := w.blob(ctx, side.blob, 1<<20)
			if err != nil {
				return "", false, err
			}
			*side.to = string(b)
		}
		files = append(files, f)
		fmt.Fprintf(&diffs, "#### %s\n\n%s\n\n", config.ShowPath(c.Path), codeBlock("diff", patch))
	}

	stat, err := w.textDiff(ctx, "--numstat", "-z", "--no-renames", base+"...refs/devbot/work")
	if err != nil {
		return "", false, err
	}
	var rest, others []string
	for _, e := range strings.Split(stat, "\x00") {
		f := strings.SplitN(e, "\t", 3)
		if len(f) != 3 || seen[f[2]] {
			continue
		}
		others = append(others, f[2])
		if f[0] == "-" {
			rest = append(rest, fmt.Sprintf("- %s (binary)", config.ShowPath(f[2])))
		} else {
			rest = append(rest, fmt.Sprintf("- %s (+%s -%s)", config.ShowPath(f[2]), f[0], f[1]))
		}
	}

	var b strings.Builder
	to := "`" + pb.branch + "`"
	if pb.pr != nil {
		to = fmt.Sprintf("`%s` (PR #%d)", pb.branch, pb.pr.Number)
	}
	held := core.HeldRef(pb.it.Ref.Number)
	rr := riskRead(files, others)
	// Why devbot can't approve this itself. The warnings count as much as
	// the flags: a workflow that already runs on devbot's branches with the
	// secrets, or that another workflow may call there, runs whatever an
	// edit makes it run without adding a flagged line. And the read parses
	// only Actions workflows, so any other protected file goes to a person.
	var why []string
	if n := len(rr.flags); n > 0 {
		why = append(why, fmt.Sprintf("the risk read below flagged %d thing(s)", n))
	}
	if len(rr.warn) > 0 {
		why = append(why, fmt.Sprintf("%s already run on devbot's branches with the repo's secrets", strings.Join(rr.warn, " and ")))
	}
	if len(rr.called) > 0 {
		why = append(why, fmt.Sprintf("%s use the repo's secrets and may run on devbot's branches, called by another workflow (`workflow_call`), which the risk read doesn't follow", strings.Join(rr.called, " and ")))
	}
	if len(rr.exposed) > 0 && len(others) > 0 {
		why = append(why, fmt.Sprintf("%s would run the %d file(s) this commit changes outside the protected paths, which nobody reviews", strings.Join(rr.exposed, " and "), len(others)))
	}
	var unread []string
	for _, f := range files {
		if !workflow(f.Path) {
			unread = append(unread, config.ShowPath(f.Path))
		}
	}
	if len(unread) > 0 {
		why = append(why, fmt.Sprintf("the risk read reads only Actions workflows, not %s", strings.Join(unread, " and ")))
	}
	clean := len(rr.flags) == 0
	pushed := auto && len(why) == 0
	switch {
	case pushed:
		fmt.Fprintf(&b, "devbot is pushing this work to %s without waiting for a person: it changes protected files that a person may approve (`approvable_paths`), "+
			"but the risk read of their diff below flagged nothing, and this repo sets `approval: %s`, so devbot approves it itself.\n\n"+
			"The commit is `%s`.\n\n", to, config.ApprovalAutoClean, head)
	default:
		fmt.Fprintf(&b, "devbot is holding this work instead of pushing it to %s: it changes protected files that a person may approve (`approvable_paths`), "+
			"and CI would run them, with the repo's secrets, as soon as they reached the branch. Nothing a model wrote runs as CI until someone approves exactly this.\n\n", to)
		if auto {
			fmt.Fprintf(&b, "This repo sets `approval: %s`, but %s, so a person approves this one.\n\n", config.ApprovalAutoClean, strings.Join(why, "; "))
		}
		fmt.Fprintf(&b, "The held commit is `%s`, kept on the forge at `%s`, a ref no CI runs on.\n\n", head, held)
	}

	b.WriteString("### Risk read\n\nAn automated read of the protected diff below. It can miss things: read the diff too.\n\n")
	if clean {
		b.WriteString("- Nothing flagged.\n")
	}
	for _, f := range rr.flags {
		b.WriteString("- " + f + "\n")
	}
	if len(rr.exposed) > 0 && len(others) > 0 {
		fmt.Fprintf(&b, "\n> **Warning:** approving this pushes the whole commit, not only the protected files. The workflows it approves (%s) run on devbot's branches or use the repo's secrets, "+
			"so as soon as it is pushed CI runs what the commit changes elsewhere (%d file(s), listed below by name only, which nobody is asked to review first) with those secrets. "+
			"Read them in the held commit before approving: `git fetch origin %s && git show FETCH_HEAD`.\n", strings.Join(rr.exposed, " and "), len(others), held)
	}
	if len(rr.warn) > 0 {
		fmt.Fprintf(&b, "\n> **Warning:** %s both run on devbot's branches and use the repo's secrets. Once this is approved, a later fix round can change "+
			"the scripts, Makefile or tests they run without touching a protected path, and that runs with the secrets with no approval asked for.\n", strings.Join(rr.warn, " and "))
	}
	if len(rr.called) > 0 {
		fmt.Fprintf(&b, "\n> **Warning:** %s can be called by other workflows (`workflow_call`) and use the repo's secrets, so they run wherever their callers do, which may be devbot's branches. "+
			"The risk read doesn't follow the callers: check them before approving.\n", strings.Join(rr.called, " and "))
	}

	b.WriteString("\n### Protected files\n\n")
	if strings.Contains(diffs.String(), "&lt;!--") {
		b.WriteString("Every `<!--` in them is shown as `&lt;!--`.\n\n")
	}
	b.WriteString(diffs.String())

	b.WriteString("### The rest of the change\n\n")
	switch {
	case len(rest) == 0:
		b.WriteString("Nothing else changes.\n\n")
	case len(rest) > 30:
		fmt.Fprintf(&b, "%s\n- and %d more\n\n", strings.Join(rest[:30], "\n"), len(rest)-30)
	default:
		b.WriteString(strings.Join(rest, "\n") + "\n\n")
	}
	if s := strings.TrimSpace(wk.out.Summary); s != "" {
		b.WriteString("The implementer's summary, in its own words (a model's, not devbot's):\n\n" + quoted(s) + "\n\n")
	}

	if pushed {
		fmt.Fprintf(&b, "### Approved by devbot\n\ndevbot records what the protected files hold as approved, as a person's approval does: "+
			"a later push that changes them is held again and read again, and the merge gate merges them only as approved. "+
			"`approval: %s` in %s has a person approve every change like this.", config.ApprovalPerson, config.RepoFile)
		return b.String(), true, nil
	}
	fmt.Fprintf(&b, "### Approving it\n\nSomeone who can direct devbot adds `%s` to this issue, or, signed in to devbot through the forge, calls `approve_task` with `sha: %s`. "+
		"devbot's own bots can't, and nor can a login listed as a trusted or required reviewer. devbot then pushes exactly this commit, and holds the protected files to this content from then on: "+
		"a later push that changes them is held again, and the merge gate merges them only as approved. "+
		"The label approves the change devbot holds when it is added: if devbot has shown a newer one here by then, it approves that one, so read the latest. `approve_task` names the commit, and is refused if it isn't the latest. "+
		"Until then the task waits here, with no timeout; `%s` starts it over, and removing the state label cancels it.",
		core.LApprove, head, core.LReady)
	return b.String(), false, nil
}

// textDiff runs git diff with args as a person reading it needs it: every
// file as text, whatever it holds, and no attributes from anywhere, so
// neither the change nor its base can hide a file's content behind
// "Binary files differ" (a "-diff" attribute) or a driver of its own.
func (w *workspace) textDiff(ctx context.Context, args ...string) (string, error) {
	empty, err := w.git(ctx, false, "hash-object", "-w", "-t", "tree", "/dev/null")
	if err != nil {
		return "", err
	}
	return w.git(ctx, false, append([]string{"-c", "core.attributesFile=/dev/null", "--attr-source=" + empty, "--literal-pathspecs", "diff", "--text"}, args...)...)
}

// unreadable lists the changes whose new content isn't text a person can
// read in a diff: binary (it has a NUL byte) or not valid UTF-8. One too
// big to read whole may be cut mid-character and listed: it is far too big
// to show anyway.
func (w *workspace) unreadable(ctx context.Context, changes []changeset.Change) ([]string, error) {
	var out []string
	for _, c := range changes {
		if strings.Trim(c.NewBlob, "0") == "" {
			continue
		}
		b, err := w.blob(ctx, c.NewBlob, 1<<20)
		if err != nil {
			return nil, err
		}
		if bytes.IndexByte(b, 0) >= 0 || !utf8.Valid(b) {
			out = append(out, config.ShowPath(c.Path))
		}
	}
	return out, nil
}

// quoted is untrusted text, defanged, as a markdown blockquote, every line
// ending made "\n" first so a lone "\r" can't end the quote early.
func quoted(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	return "> " + strings.ReplaceAll(core.Defang(s), "\n", "\n> ")
}
