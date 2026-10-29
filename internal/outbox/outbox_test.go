package outbox

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func newTestRepo(t *testing.T) *Repo {
	t.Helper()

	dir, err := os.MkdirTemp("", "squamata-outbox-")
	if err != nil {
		t.Fatalf("criar diretório: %v", err)
	}
	t.Cleanup(func() {
		for attempt := 0; attempt < 5; attempt++ {
			if err := os.RemoveAll(dir); err == nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	})

	dsn := "file:" + filepath.ToSlash(filepath.Join(dir, "outbox.db")) +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("abrir sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	repo := NewRepo(db)
	if err := repo.EnsureSchema(context.Background()); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	return repo
}

func TestEnqueueDueAndMarkSent(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	if err := repo.Enqueue(ctx, "message", "tenant-a", "sessao-1", []byte(`{"event":"message"}`)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	due, err := repo.Due(ctx, 10)
	if err != nil {
		t.Fatalf("Due: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("Due = %d eventos, esperado 1", len(due))
	}
	if due[0].Attempts != 0 {
		t.Errorf("attempts = %d, esperado 0", due[0].Attempts)
	}
	if string(due[0].Payload) != `{"event":"message"}` {
		t.Errorf("payload = %s", due[0].Payload)
	}

	if err := repo.MarkSent(ctx, due[0].ID); err != nil {
		t.Fatalf("MarkSent: %v", err)
	}
	due, _ = repo.Due(ctx, 10)
	if len(due) != 0 {
		t.Errorf("após MarkSent: %d eventos, esperado 0", len(due))
	}
}

func TestReschedulePostponesUntilDue(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	repo.SetClock(func() time.Time { return now })

	if err := repo.Enqueue(ctx, "message", "t", "s", []byte(`{}`)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	due, _ := repo.Due(ctx, 10)
	if len(due) != 1 {
		t.Fatalf("esperava 1 evento devido")
	}

	if err := repo.Reschedule(ctx, due[0].ID, 1, now.Add(30*time.Second)); err != nil {
		t.Fatalf("Reschedule: %v", err)
	}

	due, _ = repo.Due(ctx, 10)
	if len(due) != 0 {
		t.Errorf("evento reagendado não deveria estar devido agora")
	}

	now = now.Add(31 * time.Second)
	due, _ = repo.Due(ctx, 10)
	if len(due) != 1 {
		t.Fatalf("após o prazo: %d eventos, esperado 1", len(due))
	}
	if due[0].Attempts != 1 {
		t.Errorf("attempts = %d, esperado 1", due[0].Attempts)
	}
}

func TestPruneRemovesExpired(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	repo.SetClock(func() time.Time { return now })

	if err := repo.Enqueue(ctx, "message", "t", "s", []byte(`{}`)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	if removed, err := repo.Prune(ctx); err != nil || removed != 0 {
		t.Fatalf("Prune sem expirados: removidos=%d erro=%v", removed, err)
	}

	now = now.Add(DefaultTTL + time.Hour)
	removed, err := repo.Prune(ctx)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if removed != 1 {
		t.Errorf("removidos = %d, esperado 1", removed)
	}
}

func TestPruneRespectsMaxRows(t *testing.T) {
	repo := newTestRepo(t)
	repo.MaxRows = 2
	ctx := context.Background()

	for _, id := range []string{"1", "2", "3"} {
		if err := repo.Enqueue(ctx, "message", "t", "sessao-"+id, []byte(`{}`)); err != nil {
			t.Fatalf("Enqueue %s: %v", id, err)
		}
	}

	if _, err := repo.Prune(ctx); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	due, err := repo.Due(ctx, 10)
	if err != nil {
		t.Fatalf("Due: %v", err)
	}
	if len(due) != 2 {
		t.Fatalf("restaram %d eventos, esperado 2", len(due))
	}
	// O mais antigo (sessao-1) sai primeiro.
	for _, item := range due {
		if item.SessionID == "sessao-1" {
			t.Errorf("o evento mais antigo deveria ter sido descartado")
		}
	}
}

func TestBackoffIsExponentialWithCeiling(t *testing.T) {
	repo := NewRepo(nil)

	cases := map[int]time.Duration{
		0:  time.Second,
		1:  time.Second,
		2:  2 * time.Second,
		3:  4 * time.Second,
		10: 5 * time.Minute,
		20: 5 * time.Minute,
	}
	for attempts, want := range cases {
		if got := repo.Backoff(attempts); got != want {
			t.Errorf("Backoff(%d) = %v, esperado %v", attempts, got, want)
		}
	}
}
