// Package tenants guarda os consumidores do serviço (Calango-bot, Calango-food,
// marianos-meat...). Cada tenant tem o próprio token e o próprio webhook, para
// que um SaaS não veja eventos nem sessões de outro.
package tenants

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const timeLayout = time.RFC3339Nano

var (
	ErrNotFound    = errors.New("tenant não encontrado")
	ErrDuplicateID = errors.New("tenant já existe")
)

type Tenant struct {
	ID            string
	WebhookURL    string
	WebhookSecret string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type Repo struct {
	db *sql.DB
	// cache do webhook por tenant (ver webhook.go)
	cacheMu sync.Mutex
	cache   map[string]cachedWebhook
}

func NewRepo(db *sql.DB) *Repo {
	return &Repo{db: db}
}

// HashToken é o que fica gravado: o token em claro só existe no momento da criação.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (r *Repo) EnsureSchema(ctx context.Context) error {
	_, err := r.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS tenants (
		id             TEXT PRIMARY KEY,
		token_hash     TEXT NOT NULL UNIQUE,
		webhook_url    TEXT NOT NULL DEFAULT '',
		webhook_secret TEXT NOT NULL DEFAULT '',
		created_at     TEXT NOT NULL,
		updated_at     TEXT NOT NULL
	)`)
	if err != nil {
		return fmt.Errorf("criar schema de tenants: %w", err)
	}
	return nil
}

func (r *Repo) Create(ctx context.Context, id, token, webhookURL, webhookSecret string) (Tenant, error) {
	now := time.Now().UTC()

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO tenants (id, token_hash, webhook_url, webhook_secret, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		id, HashToken(token), webhookURL, webhookSecret,
		now.Format(timeLayout), now.Format(timeLayout))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return Tenant{}, ErrDuplicateID
		}
		return Tenant{}, fmt.Errorf("inserir tenant: %w", err)
	}

	return Tenant{ID: id, WebhookURL: webhookURL, WebhookSecret: webhookSecret, CreatedAt: now, UpdatedAt: now}, nil
}

func (r *Repo) Get(ctx context.Context, id string) (Tenant, error) {
	t, err := scanTenant(r.db.QueryRowContext(ctx, `
		SELECT id, webhook_url, webhook_secret, created_at, updated_at
		FROM tenants WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Tenant{}, ErrNotFound
	}
	if err != nil {
		return Tenant{}, fmt.Errorf("buscar tenant: %w", err)
	}
	return t, nil
}

// GetByToken resolve o tenant a partir do token apresentado (via hash).
func (r *Repo) GetByToken(ctx context.Context, token string) (Tenant, error) {
	t, err := scanTenant(r.db.QueryRowContext(ctx, `
		SELECT id, webhook_url, webhook_secret, created_at, updated_at
		FROM tenants WHERE token_hash = ?`, HashToken(token)))
	if errors.Is(err, sql.ErrNoRows) {
		return Tenant{}, ErrNotFound
	}
	if err != nil {
		return Tenant{}, fmt.Errorf("buscar tenant por token: %w", err)
	}
	return t, nil
}

func (r *Repo) List(ctx context.Context) ([]Tenant, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, webhook_url, webhook_secret, created_at, updated_at
		FROM tenants ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("listar tenants: %w", err)
	}
	defer rows.Close()

	list := make([]Tenant, 0)
	for rows.Next() {
		t, err := scanTenant(rows)
		if err != nil {
			return nil, fmt.Errorf("ler tenant: %w", err)
		}
		list = append(list, t)
	}
	return list, rows.Err()
}

func (r *Repo) UpdateWebhook(ctx context.Context, id, webhookURL, webhookSecret string) (Tenant, error) {
	res, err := r.db.ExecContext(ctx, `
		UPDATE tenants SET webhook_url = ?, webhook_secret = ?, updated_at = ?
		WHERE id = ?`,
		webhookURL, webhookSecret, time.Now().UTC().Format(timeLayout), id)
	if err != nil {
		return Tenant{}, fmt.Errorf("atualizar webhook do tenant: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return Tenant{}, ErrNotFound
	}
	return r.Get(ctx, id)
}

type scanner interface {
	Scan(dest ...any) error
}

func scanTenant(row scanner) (Tenant, error) {
	var (
		t                    Tenant
		createdAt, updatedAt string
	)
	if err := row.Scan(&t.ID, &t.WebhookURL, &t.WebhookSecret, &createdAt, &updatedAt); err != nil {
		return Tenant{}, err
	}
	if parsed, err := time.Parse(timeLayout, createdAt); err == nil {
		t.CreatedAt = parsed
	}
	if parsed, err := time.Parse(timeLayout, updatedAt); err == nil {
		t.UpdatedAt = parsed
	}
	return t, nil
}
