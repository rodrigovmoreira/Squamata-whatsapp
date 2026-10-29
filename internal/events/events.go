// Package events entrega os eventos do serviço ao Calango-bot por webhook
// assinado (HMAC-SHA256). O contrato está no PLANEJAMENTO.md §5.
package events

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// Nomes de evento do contrato.
const (
	SessionStatus   = "session.status"
	SessionQR       = "session.qr"
	SessionAuthFail = "session.auth_failure"
)

// maxAttempts inclui a primeira tentativa. Eventos de sessão são raros, então
// o retry aqui é simples; mensagens (F3) ganham outbox persistido.
const maxAttempts = 3

type Event struct {
	Event     string     `json:"event"`
	TenantID  string     `json:"tenantId,omitempty"`
	SessionID string     `json:"sessionId,omitempty"`
	Status    string     `json:"status,omitempty"`
	QR        string     `json:"qr,omitempty"`
	Image     string     `json:"image,omitempty"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	Message   string     `json:"message,omitempty"`
	Timestamp time.Time  `json:"timestamp"`
}

// Sink é o que o gerenciador de sessões precisa para publicar. A interface
// existe para o teste capturar eventos sem subir HTTP.
type Sink interface {
	Emit(evt Event)
}

type Dispatcher struct {
	url    string
	secret string
	client *http.Client
	logger *slog.Logger
}

// NewDispatcher devolve um dispatcher desligado quando url está vazio — é o
// caso do dev sem o Calango-bot rodando.
func NewDispatcher(url, secret string, logger *slog.Logger) *Dispatcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &Dispatcher{
		url:    url,
		secret: secret,
		client: &http.Client{Timeout: 10 * time.Second},
		logger: logger,
	}
}

func (d *Dispatcher) Enabled() bool {
	return d.url != ""
}

// Emit não bloqueia quem chamou: o handler do whatsmeow não pode ficar preso
// esperando o webhook do bot.
func (d *Dispatcher) Emit(evt Event) {
	if !d.Enabled() {
		return
	}
	evt.Timestamp = time.Now().UTC()

	payload, err := json.Marshal(evt)
	if err != nil {
		d.logger.Error("falha ao serializar evento", "evento", evt.Event, "erro", err)
		return
	}
	go d.send(payload, evt)
}

func (d *Dispatcher) send(payload []byte, evt Event) {
	backoff := time.Second
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err := d.post(payload)
		if err == nil {
			return
		}
		d.logger.Warn("falha ao entregar evento",
			"evento", evt.Event, "sessionId", evt.SessionID,
			"tentativa", attempt, "erro", err)
		if attempt == maxAttempts {
			return
		}
		time.Sleep(backoff)
		backoff *= 2
	}
}

func (d *Dispatcher) post(payload []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Signature", Signature(payload, d.secret))

	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook respondeu %d", resp.StatusCode)
	}
	return nil
}

// Signature devolve o valor de X-Signature no formato "sha256=<hex>". O bot
// recalcula sobre o corpo bruto e compara em tempo constante.
func Signature(payload []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
