package events

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
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
	dispatcher := NewDispatcher(server.URL, secret, discardLogger())

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

func TestDispatcherWithoutURLIsNoop(t *testing.T) {
	dispatcher := NewDispatcher("", "segredo", discardLogger())
	if dispatcher.Enabled() {
		t.Fatal("dispatcher sem URL deveria estar desligado")
	}
	// Não deve entrar em pânico nem abrir goroutine de envio.
	dispatcher.Emit(Event{Event: SessionStatus, SessionID: "x"})
}
