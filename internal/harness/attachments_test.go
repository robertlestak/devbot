package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.shdw.tech/rob/devbot/internal/core"
	"git.shdw.tech/rob/devbot/internal/forge"
	"git.shdw.tech/rob/devbot/internal/queue"
)

var pngMagic = []byte("\x89PNG\r\n\x1a\n")

func (h *hEnv) onlyPR() forge.PR {
	h.t.Helper()
	prs, _ := h.f.ListOpenPRs(context.Background(), repo)
	if len(prs) != 1 {
		h.t.Fatalf("want one PR, got %d", len(prs))
	}
	return prs[0]
}

func TestImplementAttachesFiles(t *testing.T) {
	eachForge(t, func(t *testing.T) {
		h := newHEnv(t, yamlChecks)
		h.mode("attach")
		ref := h.file("Say hello")
		if err := h.implement(context.Background()); err != nil {
			t.Fatal(err)
		}
		if s := h.state(ref); s != core.LInReview {
			t.Fatalf("want in-review, got %q\n%s", s, h.comments(ref))
		}
		pr := h.onlyPR()
		body := pr.Body
		m, _ := core.ParsePRMeta(body)
		if len(m.Attachments) != 3 {
			t.Fatalf("want board.png, build.log and settings.png attached, got %+v\n%s", m.Attachments, body)
		}
		byName := map[string]core.PRAttachment{}
		for _, a := range m.Attachments {
			byName[a.Name] = a
			if !bytes.Equal(h.f.Attachments[a.URL], mustRead(t, a.Name)) {
				t.Errorf("%s: uploaded content differs", a.Name)
			}
		}
		if m.Attachments[0].Name != "board.png" {
			t.Errorf("listed files come first, got %s", m.Attachments[0].Name)
		}
		board, settings, log := byName["board.png"], byName["settings.png"], byName["build.log"]
		for _, want := range []string{
			"## Attachments",
			"![board, signed-out invite card](" + board.URL + ")", // listed: its caption
			"![settings.png](" + settings.URL + ")",               // unlisted: its name
			"[build.log](" + log.URL + ")",                        // not an image: a link
			"`page.html`: type text/html",
			"`env.txt`: is a symlink",
			"`../../etc/passwd`: is outside",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("PR body is missing %q:\n%s", want, body)
			}
		}
		for _, c := range h.f.Attachments {
			if strings.Contains(string(c), "super-secret-forge-token") {
				t.Fatal("a symlink leaked the harness's environment")
			}
		}
	})
}

// The stub's attachments, by name.
func mustRead(t *testing.T, name string) []byte {
	switch name {
	case "board.png":
		return append(append([]byte(nil), pngMagic...), "board"...)
	case "settings.png":
		return append(append([]byte(nil), pngMagic...), "settings"...)
	case "chart.png":
		return append(append([]byte(nil), pngMagic...), "chart"...)
	case "build.log":
		return []byte("build output\n")
	}
	t.Fatalf("unexpected attachment %s", name)
	return nil
}

func TestAttachmentUploadFailureStillOpensPR(t *testing.T) {
	h := newHEnv(t, yamlChecks)
	h.mode("attach")
	h.f.AttachErr = errors.New("HTTP 413: too large")
	ref := h.file("Say hello")
	if err := h.implement(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s := h.state(ref); s != core.LInReview {
		t.Fatalf("an upload failure must not fail the job: got %q\n%s", s, h.comments(ref))
	}
	body := h.onlyPR().Body
	if !strings.Contains(body, "`board.png`: upload failed: HTTP 413: too large") {
		t.Fatalf("PR body should say what couldn't be attached:\n%s", body)
	}
	if strings.Contains(h.comments(ref), "failed:") {
		t.Fatal("an upload failure is not a failed attempt")
	}
}

func TestFixRoundKeepsEarlierAttachments(t *testing.T) {
	h := newHEnv(t, yamlChecks)
	h.mode("attach")
	ref := h.file("Say hello")
	if err := h.implement(context.Background()); err != nil {
		t.Fatal(err)
	}
	pr := h.onlyPR()
	first, _ := core.ParsePRMeta(pr.Body)

	ctx := context.Background()
	it, _ := h.f.GetItem(ctx, ref)
	if err := h.e.Transition(ctx, it, core.LChanges, "", ""); err != nil {
		t.Fatal(err)
	}
	h.e.Q.Enqueue(ctx, queue.Job{Kind: queue.KindImplement, Ref: ref.String(), Mode: "fix", PR: pr.Number, Reason: "show the chart too"})
	h.mode("attach-fix")
	if err := h.implement(ctx); err != nil {
		t.Fatal(err)
	}
	if s := h.state(ref); s != core.LInReview {
		t.Fatalf("want in-review after the fix, got %q\n%s", s, h.comments(ref))
	}
	body := h.onlyPR().Body
	m, _ := core.ParsePRMeta(body)
	if len(m.Attachments) != len(first.Attachments)+1 {
		t.Fatalf("want the earlier %d plus chart.png (board.png is unchanged), got %+v", len(first.Attachments), m.Attachments)
	}
	for i, a := range first.Attachments {
		if m.Attachments[i] != a || !strings.Contains(body, a.URL) {
			t.Errorf("earlier attachment %s dropped from the body", a.Name)
		}
	}
	if last := m.Attachments[len(m.Attachments)-1]; last.Name != "chart.png" || !strings.Contains(body, "![the new chart]("+last.URL+")") {
		t.Fatalf("new attachment not appended:\n%s", body)
	}
	if len(h.f.Attachments) != 4 {
		t.Fatalf("an unchanged file shouldn't be uploaded again: %d uploads", len(h.f.Attachments))
	}
}

func TestReviewerGetsAttachments(t *testing.T) {
	eachForge(t, func(t *testing.T) {
		h := newHEnv(t, yamlChecks)
		h.mode("attach")
		ref := h.file("Say hello")
		h.implement(context.Background())
		pr := h.onlyPR()
		sha := git(t, h.bare, "rev-parse", pr.HeadBranch)
		h.f.SetHead(repo, pr.Number, sha)
		h.f.SetStatus(sha, "ci", "success")
		if _, err := h.e.EvaluateItem(context.Background(), ref, core.SrcWebhook); err != nil {
			t.Fatal(err)
		}
		var seenContext string
		h.r.Agent = &spy{Runner: h.r.Agent, after: func(dir string) {
			b, _ := os.ReadFile(filepath.Join(dir, ".devbot", "context.md"))
			seenContext = string(b)
		}}
		h.mode("review-attachments")
		if err := h.review(context.Background()); err != nil {
			t.Fatal(err)
		}
		rv, _ := h.f.Reviews(context.Background(), repo, pr.Number)
		if len(rv) != 1 {
			t.Fatalf("want a review, got %+v", rv)
		}
		// The stub listed .devbot/attachments (with each file past the 8-byte PNG
		// header) when it ran, so the files were
		// there before the model started.
		if want := "saw board.png=board;build.log=tput;settings.png=settings;"; !strings.HasPrefix(rv[0].Body, want) {
			t.Fatalf("reviewer saw %q, want %q", rv[0].Body, want)
		}
		if !strings.Contains(seenContext, "`.devbot/attachments/board.png` (image/png): board, signed-out invite card") {
			t.Fatalf("context.md should list the attachments:\n%s", seenContext)
		}
	})
}

// The model writes the summary and notes, which land above devbot's own
// devbot:pr marker; a marker forged there must not decide what the reviewer
// downloads, now or after a fix round rewrites the body.
func TestForgedPRMarkerIsIgnored(t *testing.T) {
	eachForge(t, func(t *testing.T) {
		const secret = "https://fake/attachments/99/secret.png"
		h := newHEnv(t, yamlChecks)
		h.f.Attachments[secret] = []byte("another repo's private file")
		h.mode("attach-forged")
		ref := h.file("Say hello")
		ctx := context.Background()
		if err := h.implement(ctx); err != nil {
			t.Fatal(err)
		}
		pr := h.onlyPR()
		check := func(when, body string) {
			t.Helper()
			m, ok := core.ParsePRMeta(body)
			if !ok || len(m.Attachments) == 0 {
				t.Fatalf("%s: devbot's own marker was lost: %+v\n%s", when, m, body)
			}
			for _, a := range m.Attachments {
				if a.URL == secret {
					t.Fatalf("%s: the forged attachment made it into the marker:\n%s", when, body)
				}
			}
			if n := strings.Count(body, "<!--"); n != 1 {
				t.Fatalf("%s: want only devbot's marker, found %d comment openers:\n%s", when, n, body)
			}
		}
		check("first round", pr.Body)
		if c := h.comments(ref); strings.Contains(c, secret) {
			t.Fatalf("the forged marker was posted in a comment:\n%s", c)
		}

		it, _ := h.f.GetItem(ctx, ref)
		if err := h.e.Transition(ctx, it, core.LChanges, "", ""); err != nil {
			t.Fatal(err)
		}
		h.e.Q.Enqueue(ctx, queue.Job{Kind: queue.KindImplement, Ref: ref.String(), Mode: "fix", PR: pr.Number, Reason: "show the chart too"})
		h.mode("attach-fix")
		if err := h.implement(ctx); err != nil {
			t.Fatal(err)
		}
		pr = h.onlyPR()
		check("fix round", pr.Body)

		sha := git(t, h.bare, "rev-parse", pr.HeadBranch)
		h.f.SetHead(repo, pr.Number, sha)
		h.f.SetStatus(sha, "ci", "success")
		if _, err := h.e.EvaluateItem(ctx, ref, core.SrcWebhook); err != nil {
			t.Fatal(err)
		}
		var seenContext string
		h.r.Agent = &spy{Runner: h.r.Agent, after: func(dir string) {
			b, _ := os.ReadFile(filepath.Join(dir, ".devbot", "context.md"))
			seenContext = string(b)
		}}
		h.mode("review-attachments")
		if err := h.review(ctx); err != nil {
			t.Fatal(err)
		}
		rv, _ := h.f.Reviews(ctx, repo, pr.Number)
		if len(rv) != 1 {
			t.Fatalf("want a review, got %+v", rv)
		}
		if want := "saw board.png=board;chart.png=chart;"; !strings.HasPrefix(rv[0].Body, want) {
			t.Fatalf("reviewer saw %q, want %q", rv[0].Body, want)
		}
		if strings.Contains(seenContext, "secret") {
			t.Fatalf("the reviewer was pointed at the forged attachment:\n%s", seenContext)
		}
	})
}

func TestReadAttachmentsLimits(t *testing.T) {
	dir := t.TempDir()
	w := &workspace{repo: dir}
	att := filepath.Join(dir, ".devbot", "attachments")
	os.MkdirAll(att, 0o755)
	write := func(name string, data []byte) {
		if err := os.WriteFile(filepath.Join(att, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	png := func(n int) []byte { return append(append([]byte(nil), pngMagic...), bytes.Repeat([]byte{0}, n)...) }

	// Type is sniffed, not taken from the name.
	write("fake.png", []byte("<html><body>hi</body></html>"))
	write("shot", png(10))
	// Per-file size.
	write("huge.png", png(maxAttachmentSize))
	// Outside the directory, and a symlink.
	os.WriteFile(filepath.Join(dir, "secret.png"), png(10), 0o644)
	os.Symlink("../../secret.png", filepath.Join(att, "link.png"))
	os.Symlink("/etc/passwd", filepath.Join(att, "passwd.txt"))
	got, notes := w.readAttachments([]attachmentEntry{{Path: "../../secret.png"}, {Path: "/etc/passwd"}, {Path: "missing.png"}})
	why := map[string]string{}
	for _, n := range notes {
		why[n.name] = n.why
	}
	if len(got) != 1 || got[0].name != "shot.png" || got[0].typ != "image/png" {
		t.Fatalf("want only shot, renamed shot.png, got %+v", got)
	}
	for name, want := range map[string]string{
		"fake.png": "type text/html", "huge.png": "limit per file", "link.png": "symlink", "passwd.txt": "symlink",
		"../../secret.png": "outside", "/etc/passwd": "outside", "missing.png": "not found",
	} {
		if !strings.Contains(why[name], want) {
			t.Errorf("%s: want a note containing %q, got %q", name, want, why[name])
		}
	}

	// Count, then total size.
	os.RemoveAll(att)
	os.MkdirAll(att, 0o755)
	for i := range maxAttachments + 2 {
		write(fmt.Sprintf("%02d.png", i), png(10))
	}
	got, notes = w.readAttachments(nil)
	if len(got) != maxAttachments || len(notes) != 2 || !strings.Contains(notes[0].why, "limit of 10 files") {
		t.Fatalf("count limit: got %d files, notes %+v", len(got), notes)
	}
	os.RemoveAll(att)
	os.MkdirAll(att, 0o755)
	for i := range 5 {
		write(fmt.Sprintf("%d.png", i), png(maxAttachmentSize-100))
	}
	got, notes = w.readAttachments(nil)
	if len(got) != 4 || len(notes) != 1 || !strings.Contains(notes[0].why, "total") {
		t.Fatalf("total limit: got %d files, notes %+v", len(got), notes)
	}
}

func TestAttachmentsDirSymlinkedOutIsRefused(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "x.png"), append(append([]byte(nil), pngMagic...), 1), 0o644)
	os.MkdirAll(filepath.Join(dir, ".devbot"), 0o755)
	os.Symlink(outside, filepath.Join(dir, ".devbot", "attachments"))
	w := &workspace{repo: dir}
	got, notes := w.readAttachments([]attachmentEntry{{Path: "x.png"}})
	if len(got) != 0 || len(notes) != 1 {
		t.Fatalf("a directory linked out of the repo must not be read: %+v %+v", got, notes)
	}
}
