// Package outbox guarda os eventos que ainda não foram entregues ao
// Calango-bot. É o que impede perder mensagem quando o bot está fora do ar
// (decisão D8).
package outbox

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

const (
	// DefaultMaxRows limita o tamanho da fila (D8: 5 000 linhas).
	DefaultMaxRows = 5000
	// DefaultTTL é a idade máxima de um evento pendente (D8: 7 dias).
	DefaultTTL = 7 * 24 * time.Hour
	// MaxAttempts é o teto de tentativas antes de descartar (D8: 10).
	MaxAttempts = 10

	timeLayout = time.RFC3339Nano
)

type Pending struct {
	ID        int64
	EventType string
	TenantID  string
	SessionID string
	Payload   []byte
	Attempts  int
}

type Repo struct {
	db      *sql.DB
	MaxRows int
	TTL     time.Duration
	// now é injetável para o teste controlar idade e backoff.
	now func() time.Time
}

func NewRepo(db *sql.DB) *Repo {
	return &Repo{
		db:      db,
		MaxRows: DefaultMaxRows,
		TTL:     DefaultTTL,
		now:     func() time.Time { return time.Now().UTC() },
	}
}

// SetClock troca a fonte de tempo (usado nos testes).
func (r *Repo) SetClock(now func() time.Time) {
	r.now = now
}

// EnsureSchema cria a tabela sem usar PRAGMA user_version: esse contador
// pertence ao pacote sessions (metadados de sessão) e não deve ser disputado
// por dois pacotes.
func (r *Repo) EnsureSchema(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS outbox (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			event_type      TEXT NOT NULL,
			tenant_id       TEXT NOT NULL,
			session_id      TEXT NOT NULL,
			payload         BLOB NOT NULL,
			attempts        INTEGER NOT NULL DEFAULT 0,
			next_attempt_at TEXT NOT NULL,
			created_at      TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_outbox_due ON outbox (next_attempt_at)`,
	}
	for _, stmt := range statements {
		if _, err := r.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("criar schema do outbox: %w", err)
		}
	}
	return nil
}

func (r *Repo) Enqueue(ctx context.Context, eventType, tenantID, sessionID string, payload []byte) error {
	now := r.now()
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO outbox (event_type, tenant_id, session_id, payload, attempts, next_attempt_at, created_at)
		VALUES (?, ?, ?, ?, 0, ?, ?)`,
		eventType, tenantID, sessionID, payload, now.Format(timeLayout), now.Format(timeLayout),
	)
	if err != nil {
		return fmt.Errorf("enfileirar evento: %w", err)
	}
	return nil
}

func (r *Repo) Due(ctx context.Context, limit int) ([]Pending, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, event_type, tenant_id, session_id, payload, attempts
		FROM outbox WHERE next_attempt_at <= ?
		ORDER BY next_attempt_at LIMIT ?`, r.now().Format(timeLayout), limit)
	if err != nil {
		return nil, fmt.Errorf("listar eventos pendentes: %w", err)
	}
	defer rows.Close()

	due := make([]Pending, 0)
	for rows.Next() {
		var p Pending
		if err := rows.Scan(&p.ID, &p.EventType, &p.TenantID, &p.SessionID, &p.Payload, &p.Attempts); err != nil {
			return nil, fmt.Errorf("ler evento pendente: %w", err)
		}
		due = append(due, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("percorrer eventos: %w", err)
	}
	return due, nil
}

func (r *Repo) MarkSent(ctx context.Context, id int64) error {
	if _, err := r.db.ExecContext(ctx, `DELETE FROM outbox WHERE id = ?`, id); err != nil {
		return fmt.Errorf("remover evento entregue: %w", err)
	}
	return nil
}

func (r *Repo) Reschedule(ctx context.Context, id int64, attempts int, next time.Time) error {
	if _, err := r.db.ExecContext(ctx, `
		UPDATE outbox SET attempts = ?, next_attempt_at = ? WHERE id = ?`,
		attempts, next.UTC().Format(timeLayout), id); err != nil {
		return fmt.Errorf("reagendar evento: %w", err)
	}
	return nil
}

// Prune descarta o que passou do TTL e o excedente acima de MaxRows (o mais
// antigo primeiro). Devolve quantas linhas saíram.
func (r *Repo) Prune(ctx context.Context) (int64, error) {
	cutoff := r.now().Add(-r.TTL).Format(timeLayout)

	res, err := r.db.ExecContext(ctx, `DELETE FROM outbox WHERE created_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("limpar outbox expirado: %w", err)
	}
	removed, _ := res.RowsAffected()

	res, err = r.db.ExecContext(ctx, `
		DELETE FROM outbox WHERE id IN (
			SELECT id FROM outbox ORDER BY id DESC LIMIT -1 OFFSET ?
		)`, r.MaxRows)
	if err != nil {
		return removed, fmt.Errorf("limitar tamanho do outbox: %w", err)
	}
	extra, _ := res.RowsAffected()
	return removed + extra, nil
}

// Backoff é exponencial a partir de 1 s, com teto de 5 min (D8).
func (r *Repo) Backoff(attempts int) time.Duration {
	if attempts < 1 {
		return time.Second
	}
	delay := time.Second << (attempts - 1)
	if delay > 5*time.Minute {
		return 5 * time.Minute
	}
	return delay
}
