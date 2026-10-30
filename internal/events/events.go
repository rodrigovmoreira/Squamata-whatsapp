// Package events entrega os eventos do serviço ao Calango-bot por webhook
// assinado (HMAC-SHA256). O contrato está no PLANEJAMENTO.md §5.
//
// Com outbox configurado, todo evento é persistido antes do envio: o bot pode
// ficar fora do ar sem que a mensagem se perca (decisão D8).
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

	"github.com/rodrigovmoreira/Squamata-whatsapp/internal/outbox"
)

// Nomes de evento do contrato.
const (
	SessionStatus   = "session.status"
	SessionQR       = "session.qr"
	SessionAuthFail = "session.auth_failure"
	Message         = "message"
	MessageSent     = "message.sent"
)

const (
	// maxAttempts inclui a primeira tentativa.
	maxAttempts = outbox.MaxAttempts
	drainEvery  = 2 * time.Second
	pruneEvery  = 60 * time.Second
	drainBatch  = 20
)

type Event struct {
	Event     string `json:"event"`
	TenantID  string `json:"tenantId,omitempty"`
	SessionID string `json:"sessionId,omitempty"`

	// Sessão
	Status    string     `json:"status,omitempty"`
	QR        string     `json:"qr,omitempty"`
	Image     string     `json:"image,omitempty"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`

	// Mensagem
	MessageID string `json:"messageId,omitempty"`
	From      string `json:"from,omitempty"`
	RawFrom   string `json:"rawFrom,omitempty"`
	PushName  string `json:"pushName,omitempty"`
	Type      string `json:"type,omitempty"`
	Text      string `json:"text,omitempty"`
	Media     *Media `json:"media,omitempty"`
	FromMe    bool   `json:"fromMe,omitempty"`

	// Descrição legível de falha (usada em session.auth_failure e status error).
	Message string `json:"message,omitempty"`

	// Timestamp é o horário do fato: o da mensagem no WhatsApp quando houver,
	// senão o instante da emissão.
	Timestamp time.Time `json:"timestamp"`
}

// Media carrega os bytes apenas de image/audio (D1). Vídeo e documento vão só
// como metadados; acima de 8 MB a mensagem sai sem bytes e com TooLarge.
type Media struct {
	Mime     string  `json:"mime,omitempty"`
	Filename string  `json:"filename,omitempty"`
	Size     uint64  `json:"size,omitempty"`
	Seconds  *uint32 `json:"seconds,omitempty"`
	Base64   string  `json:"base64,omitempty"`
	TooLarge bool    `json:"tooLarge,omitempty"`
}

// Sink é o que o gerenciador de sessões precisa para publicar. A interface
// existe para o teste capturar eventos sem subir HTTP.
type Sink interface {
	Emit(evt Event)
}

type Dispatcher struct {
	// url e secret são o destino padrão (SQUAMATA_WEBHOOK_URL). Servem de
	// fallback quando o tenant não tem webhook próprio.
	url    string
	secret string
	client *http.Client
	logger *slog.Logger
	// queue é opcional: sem ela o envio é só em memória (dev e testes).
	queue *outbox.Repo
	// resolver devolve o webhook do tenant (multi-SaaS). Pode ser nil.
	resolver WebhookResolver
}

// WebhookResolver permite que cada tenant receba os eventos na própria URL.
type WebhookResolver interface {
	Webhook(ctx context.Context, tenantID string) (url, secret string)
}

// NewDispatcher devolve um dispatcher desligado quando url está vazio e não há
// resolver — é o caso do dev sem consumidor rodando.
func NewDispatcher(url, secret string, queue *outbox.Repo, logger *slog.Logger) *Dispatcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &Dispatcher{
		url:    url,
		secret: secret,
		client: &http.Client{Timeout: 10 * time.Second},
		logger: logger,
		queue:  queue,
	}
}

// WithResolver liga a resolução de webhook por tenant.
func (d *Dispatcher) WithResolver(resolver WebhookResolver) *Dispatcher {
	d.resolver = resolver
	return d
}

func (d *Dispatcher) Enabled() bool {
	return d.url != "" || d.resolver != nil
}

// target decide para onde um evento vai: webhook do tenant ou o padrão.
func (d *Dispatcher) target(tenantID string) (string, string, bool) {
	if d.resolver != nil && tenantID != "" {
		if url, secret := d.resolver.Webhook(context.Background(), tenantID); url != "" {
			return url, secret, true
		}
	}
	if d.url == "" {
		return "", "", false
	}
	return d.url, d.secret, true
}

// Emit não bloqueia quem chamou: o handler do whatsmeow não pode ficar preso
// esperando o webhook do bot.
func (d *Dispatcher) Emit(evt Event) {
	if !d.Enabled() {
		return
	}
	if evt.Timestamp.IsZero() {
		evt.Timestamp = time.Now().UTC()
	}

	payload, err := json.Marshal(evt)
	if err != nil {
		d.logger.Error("falha ao serializar evento", "evento", evt.Event, "erro", err)
		return
	}

	if d.queue != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := d.queue.Enqueue(ctx, evt.Event, evt.TenantID, evt.SessionID, payload)
		cancel()
		if err == nil {
			return
		}
		d.logger.Error("falha ao gravar evento no outbox; enviando direto",
			"evento", evt.Event, "sessionId", evt.SessionID, "erro", err)
	}

	go d.send(payload, evt)
} // Run drena o outbox e faz a limpeza periódica. Sem outbox ou sem URL, não faz
// nada — o processo não precisa desse loop em dev.
func (d *Dispatcher) Run(ctx context.Context) {
	if d.queue == nil || !d.Enabled() {
		return
	}

	drain := time.NewTicker(drainEvery)
	prune := time.NewTicker(pruneEvery)
	defer drain.Stop()
	defer prune.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-drain.C:
			d.drain(ctx)
		case <-prune.C:
			if removed, err := d.queue.Prune(ctx); err != nil {
				d.logger.Warn("falha ao limpar outbox", "erro", err)
			} else if removed > 0 {
				d.logger.Info("outbox limpo", "removidos", removed)
			}
		}
	}
}

func (d *Dispatcher) drain(ctx context.Context) {
	pending, err := d.queue.Due(ctx, drainBatch)
	if err != nil {
		d.logger.Warn("falha ao ler outbox", "erro", err)
		return
	}

	for _, item := range pending {
		url, secret, ok := d.target(item.TenantID)
		if !ok {
			d.logger.Error("nenhum webhook configurado para o tenant; evento descartado",
				"evento", item.EventType, "tenantId", item.TenantID)
			_ = d.queue.MarkSent(ctx, item.ID)
			continue
		}

		if err := d.post(url, secret, item.Payload); err == nil {
			if err := d.queue.MarkSent(ctx, item.ID); err != nil {
				d.logger.Warn("falha ao remover evento entregue", "id", item.ID, "erro", err)
			}
			continue
		} else {
			attempts := item.Attempts + 1
			if attempts >= maxAttempts {
				d.logger.Error("evento descartado após o limite de tentativas",
					"evento", item.EventType, "sessionId", item.SessionID, "tentativas", attempts, "erro", err)
				_ = d.queue.MarkSent(ctx, item.ID)
				continue
			}
			next := time.Now().UTC().Add(d.queue.Backoff(attempts))
			if err := d.queue.Reschedule(ctx, item.ID, attempts, next); err != nil {
				d.logger.Warn("falha ao reagendar evento", "id", item.ID, "erro", err)
			}
			d.logger.Warn("evento não entregue; reagendado",
				"evento", item.EventType, "sessionId", item.SessionID,
				"tentativa", attempts, "proxima", next, "erro", err)
		}
	}
}

func (d *Dispatcher) send(payload []byte, evt Event) {
	url, secret, ok := d.target(evt.TenantID)
	if !ok {
		d.logger.Error("nenhum webhook configurado para o tenant; evento não enviado",
			"evento", evt.Event, "tenantId", evt.TenantID)
		return
	}

	backoff := time.Second
	for attempt := 1; attempt <= 3; attempt++ {
		err := d.post(url, secret, payload)
		if err == nil {
			return
		}
		d.logger.Warn("falha ao entregar evento",
			"evento", evt.Event, "sessionId", evt.SessionID, "tenantId", evt.TenantID,
			"tentativa", attempt, "erro", err)
		if attempt == 3 {
			return
		}
		time.Sleep(backoff)
		backoff *= 2
	}
}

func (d *Dispatcher) post(url, secret string, payload []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Signature", Signature(payload, secret))

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
