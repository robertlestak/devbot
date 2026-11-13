package queue

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"git.shdw.tech/rob/devbot/internal/forge"
)

// A note that crossed a newer one on the way can't make a spent limit look
// unspent; a later window replaces an earlier one.
func TestNoteQuotaOnlyMovesForward(t *testing.T) {
	s := New(miniredis.RunT(t).Addr(), 0)
	ctx := context.Background()
	reset := time.Unix(time.Now().Add(time.Hour).Unix(), 0)
	get := func() forge.Quota {
		t.Helper()
		qs, err := s.Quotas(ctx, "github", "installation 1", "core", "search")
		if err != nil || len(qs) != 2 || qs[1] != (forge.Quota{}) {
			t.Fatalf("%v %v", qs, err)
		}
		return qs[0]
	}
	for _, q := range []forge.Quota{
		{Remaining: 10, Limit: 5000, Reset: reset},
		{Remaining: 0, Limit: 5000, Reset: reset},
		{Remaining: 4, Limit: 5000, Reset: reset},
		{Remaining: 4999, Limit: 5000, Reset: reset.Add(-time.Hour)},
	} {
		if err := s.NoteQuota(ctx, "github", "installation 1", "core", q); err != nil {
			t.Fatal(err)
		}
	}
	if q := get(); q.Remaining != 0 || q.Limit != 5000 || !q.Reset.Equal(reset) {
		t.Fatalf("got %+v", q)
	}
	next := forge.Quota{Remaining: 4999, Limit: 5000, Reset: reset.Add(time.Hour)}
	s.NoteQuota(ctx, "github", "installation 1", "core", next)
	if q := get(); q != next {
		t.Fatalf("got %+v, want the next window", q)
	}
	all, err := s.AllQuotas(ctx, "github")
	if err != nil || len(all) != 1 || all[[2]string{"installation 1", "core"}] != next {
		t.Fatalf("%v %v", all, err)
	}
}

// Kept events about one thing coalesce into one, at the later time with
// the most tries; one past its tries is dropped; each is taken once, when
// due.
func TestLater(t *testing.T) {
	s := New(miniredis.RunT(t).Addr(), 0)
	ctx := context.Background()
	now := time.Now()
	later := func(key, payload string, at time.Time, tries int) string {
		t.Helper()
		out, err := s.Later(ctx, key, []byte(payload), at, tries, 3)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if later("a", "a1", now.Add(time.Minute), 2) != LaterKept || later("a", "a2", now.Add(time.Second), 1) != LaterCoalesced ||
		later("b", "b1", now.Add(time.Second), 1) != LaterKept {
		t.Fatal("outcomes")
	}
	if n, _ := s.LaterCount(ctx); n != 2 {
		t.Fatalf("%d kept", n)
	}
	got, err := s.TakeLater(ctx, now.Add(2*time.Second), 10)
	if err != nil || len(got) != 1 || string(got[0].Payload) != "b1" || got[0].Tries != 1 {
		t.Fatalf("%+v %v", got, err)
	}
	got, _ = s.TakeLater(ctx, now.Add(2*time.Minute), 10)
	if len(got) != 1 || string(got[0].Payload) != "a2" || got[0].Tries != 2 {
		t.Fatalf("want the newer payload at the later time with the most tries: %+v", got)
	}
	if got, _ := s.TakeLater(ctx, now.Add(time.Hour), 10); len(got) != 0 {
		t.Fatalf("taken twice: %+v", got)
	}

	later("c", "c1", now, 3)
	if later("c", "c2", now, 4) != LaterDropped {
		t.Fatal("past its tries an event must be dropped")
	}
	if n, _ := s.LaterCount(ctx); n != 0 {
		t.Fatalf("%d kept after the drop", n)
	}
}
