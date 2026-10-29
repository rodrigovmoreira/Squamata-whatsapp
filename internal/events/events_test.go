package events

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rodrigovmoreira/Squamata-whatsapp/internal/outbox"

	_ "modernc.org/sqlite"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestQueue(t *testing.T) *outbox.Repo {
	t.Helper()

	dir, err := os.MkdirTemp("", "squamata-events-")
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

	dsn := "file:" + filepath.ToSlash(filepath.Join(dir, "events.db")) +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("abrir sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	queue := outbox.NewRepo(db)
	if err := queue.EnsureSchema(context.Background()); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	return queue
}

func TestSignatureIsStableHMAC(t *testing.T) {
	payload := []byte(`{"event":"session.status"}`)

	first := Signature(payload, "segredo")
	second := Signature(payload, "segredo")
	if first != second {
		t.Fatalf("assinatura não determinística: %q != %q", first, second)
	}
	if other := Signature(payload, "outro"); other == first {
		t.Error("segredo diferente deveria gerar assinatura diferente")
	}
	if len(first) != len("sha256=")+64 {
		t.Errorf("formato inesperado: %q", first)
	}
}

func TestDispatcherDeliversSignedEvent(t *testing.T) {
	received := make(chan *http.Request, 1)
	body := make(chan []byte, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body <- raw
		received <- r
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	const secret = "segredo-do-webhook"
	dispatcher := NewDispatcher(server.URL, secret, nil, discardLogger())

	expires := time.Now().UTC().Add(20 * time.Second)
	dispatcher.Emit(Event{
		Event:     SessionQR,
		TenantID:  "tenant-a",
		SessionID: "sessao-1",
		QR:        "2@payload",
		Image:     "data:image/png;base64,AAA",
		ExpiresAt: &expires,
	})

	select {
	case req := <-received:
		raw := <-body
		if got, want := req.Header.Get("X-Signature"), Signature(raw, secret); got != want {
			t.Errorf("X-Signature = %q, esperado %q", got, want)
		}
		if ct := req.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q", ct)
		}

		var evt Event
		if err := json.Unmarshal(raw, &evt); err != nil {
			t.Fatalf("payload não é JSON: %v", err)
		}
		if evt.Event != SessionQR || evt.SessionID != "sessao-1" || evt.QR != "2@payload" {
			t.Errorf("evento inesperado: %+v", evt)
		}
		if evt.Timestamp.IsZero() {
			t.Error("timestamp não foi preenchido")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("evento não chegou ao webhook")
	}
}

func TestDispatcherKeepsMessageTimestamp(t *testing.T) {
	body := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body <- raw
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	dispatcher := NewDispatcher(server.URL, "s", nil, discardLogger())
	original := time.Date(2026, 10, 29, 12, 30, 0, 0, time.UTC)
	dispatcher.Emit(Event{Event: Message, MessageID: "ABC", Timestamp: original})

	select {
	case raw := <-body:
		var evt Event
		if err := json.Unmarshal(raw, &evt); err != nil {
			t.Fatalf("payload não é JSON: %v", err)
		}
		if !evt.Timestamp.Equal(original) {
			t.Errorf("timestamp = %v, esperado %v (o horário da mensagem não pode ser trocado)", evt.Timestamp, original)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("evento não chegou")
	}
}

func TestDispatcherEnqueuesBeforeSending(t *testing.T) {
	queue := newTestQueue(t)
	ctx := context.Background()

	delivered := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		delivered <- struct{}{}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	dispatcher := NewDispatcher(server.URL, "s", queue, discardLogger())
	dispatcher.Emit(Event{Event: Message, SessionID: "sessao-1", Text: "oi"})

	// Antes do drain o evento está persistido: é o que garante D8.
	due, err := queue.Due(ctx, 10)
	if err != nil {
		t.Fatalf("Due: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("outbox = %d eventos, esperado 1", len(due))
	}

	dispatcher.drain(ctx)

	select {
	case <-delivered:
	case <-time.After(5 * time.Second):
		t.Fatal("drain não entregou o evento")
	}

	due, _ = queue.Due(ctx, 10)
	if len(due) != 0 {
		t.Errorf("após drain: %d eventos no outbox, esperado 0", len(due))
	}
}

func TestDispatcherReschedulesOnFailure(t *testing.T) {
	queue := newTestQueue(t)
	ctx := context.Background()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	dispatcher := NewDispatcher(server.URL, "s", queue, discardLogger())
	dispatcher.Emit(Event{Event: Message, SessionID: "sessao-1"})

	dispatcher.drain(ctx)

	// O evento não pode ser descartado: continua na fila, reagendado.
	due, err := queue.Due(ctx, 10)
	if err != nil {
		t.Fatalf("Due: %v", err)
	}
	if len(due) != 0 {
		t.Errorf("evento reagendado não deveria estar devido agora")
	}

	queue.SetClock(func() time.Time { return time.Now().UTC().Add(2 * time.Minute) })
	due, err = queue.Due(ctx, 10)
	if err != nil {
		t.Fatalf("Due após avançar o relógio: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("evento deveria continuar na fila, veio %d", len(due))
	}
	if due[0].Attempts != 1 {
		t.Errorf("attempts = %d, esperado 1", due[0].Attempts)
	}
}

func TestDispatcherWithoutURLIsNoop(t *testing.T) {
	dispatcher := NewDispatcher("", "segredo", nil, discardLogger())
	if dispatcher.Enabled() {
		t.Fatal("dispatcher sem URL deveria estar desligado")
	}
	dispatcher.Emit(Event{Event: SessionStatus, SessionID: "x"})
}
