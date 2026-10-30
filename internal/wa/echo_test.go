package wa

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

func newTestManager() *Manager {
	return &Manager{
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		clients: make(map[string]*managed),
		sent:    make(map[string]time.Time),
	}
}

// TestEchoDedup é a proteção contra duplicar no CRM a mensagem que a própria
// API enviou: o eco de `message.sent` é consumido uma única vez.
func TestEchoDedup(t *testing.T) {
	m := newTestManager()

	m.rememberSent("sessao-1", "ABC123")

	if !m.consumeSent("sessao-1", "ABC123") {
		t.Fatal("primeiro eco deveria ser descartado")
	}
	if m.consumeSent("sessao-1", "ABC123") {
		t.Error("segundo eco do mesmo ID não deveria ser descartado de novo")
	}
}

func TestEchoDedupIsPerSession(t *testing.T) {
	m := newTestManager()
	m.rememberSent("sessao-1", "ABC123")

	if m.consumeSent("sessao-2", "ABC123") {
		t.Error("ID de outra sessão não deveria ser consumido")
	}
}

func TestEchoDedupIgnoresEmptyID(t *testing.T) {
	m := newTestManager()

	m.rememberSent("sessao-1", "")
	if m.consumeSent("sessao-1", "") {
		t.Error("ID vazio nunca deveria ser consumido")
	}
}

func TestEchoDedupExpires(t *testing.T) {
	m := newTestManager()

	m.sentMu.Lock()
	m.sent["sessao-1|ANTIGO"] = time.Now().Add(-2 * sentTTL)
	m.sentMu.Unlock()

	m.rememberSent("sessao-1", "NOVO")

	m.sentMu.Lock()
	_, exists := m.sent["sessao-1|ANTIGO"]
	m.sentMu.Unlock()

	if exists {
		t.Error("registro expirado deveria ter sido limpo")
	}
	if !m.consumeSent("sessao-1", "NOVO") {
		t.Error("registro recente deveria continuar válido")
	}
}
