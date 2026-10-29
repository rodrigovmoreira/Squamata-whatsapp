package sessions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

const (
	schemaVersion = 1
	// RFC3339Nano preserva ordem lexicográfica e mantém o fuso explícito.
	timeLayout = time.RFC3339Nano
)

var createStatements = []string{
	`CREATE TABLE IF NOT EXISTS sessions (
		id            TEXT PRIMARY KEY,
		tenant_id     TEXT NOT NULL,
		owner_user_id TEXT NOT NULL DEFAULT '',
		name          TEXT NOT NULL,
		phone         TEXT NOT NULL DEFAULT '',
		status        TEXT NOT NULL,
		last_error    TEXT NOT NULL DEFAULT '',
		connected_at  TEXT,
		created_at    TEXT NOT NULL,
		updated_at    TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_sessions_tenant_created
		ON sessions (tenant_id, created_at)`,
}

// Repo guarda os metadados das sessões no mesmo arquivo SQLite do sqlstore.
type Repo struct {
	db *sql.DB
}

func NewRepo(db *sql.DB) *Repo {
	return &Repo{db: db}
}

// Migrate aplica o schema do serviço usando PRAGMA user_version, que é o
// mecanismo nativo do SQLite e não conflita com as migrações do whatsmeow
// (elas controlam apenas as tabelas whatsmeow_*).
func (r *Repo) Migrate(ctx context.Context) error {
	var current int
	if err := r.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("ler user_version: %w", err)
	}
	if current >= schemaVersion {
		return nil
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("abrir transação de migração: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, stmt := range createStatements {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("aplicar schema de sessões: %w", err)
		}
	}
	// PRAGMA não aceita placeholder; o valor é uma constante do pacote.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return fmt.Errorf("gravar user_version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("confirmar migração: %w", err)
	}
	return nil
}

func (r *Repo) Create(ctx context.Context, in Session) (Session, error) {
	if err := in.Normalize(); err != nil {
		return Session{}, err
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	s := in
	s.ID = uuid.NewString()
	s.Status = StatusDisconnected
	s.LastError = ""
	s.ConnectedAt = nil
	s.CreatedAt = now
	s.UpdatedAt = now

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO sessions (id, tenant_id, owner_user_id, name, phone, status,
			last_error, connected_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, NULL, ?, ?)`,
		s.ID, s.TenantID, s.OwnerUserID, s.Name, s.Phone, string(s.Status), s.LastError,
		s.CreatedAt.Format(timeLayout), s.UpdatedAt.Format(timeLayout),
	)
	if err != nil {
		return Session{}, fmt.Errorf("inserir sessão: %w", err)
	}
	return s, nil
}

// Get sempre filtra por tenant: um sessionId de outro tenant não resolve.
func (r *Repo) Get(ctx context.Context, tenantID, id string) (Session, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, tenant_id, owner_user_id, name, phone, status, last_error,
			connected_at, created_at, updated_at
		FROM sessions WHERE tenant_id = ? AND id = ?`, tenantID, id)

	s, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("buscar sessão: %w", err)
	}
	return s, nil
}

func (r *Repo) List(ctx context.Context, tenantID string) ([]Session, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, tenant_id, owner_user_id, name, phone, status, last_error,
			connected_at, created_at, updated_at
		FROM sessions WHERE tenant_id = ? ORDER BY created_at`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("listar sessões: %w", err)
	}
	defer rows.Close()

	list := make([]Session, 0)
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("ler sessão: %w", err)
		}
		list = append(list, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("percorrer sessões: %w", err)
	}
	return list, nil
}

// UpdateStatus é usado pela máquina de estados do whatsmeow (F2 em diante).
func (r *Repo) UpdateStatus(ctx context.Context, tenantID, id string, status Status, lastError string) error {
	if !status.Valid() {
		return ErrInvalidStatus
	}

	res, err := r.db.ExecContext(ctx, `
		UPDATE sessions SET status = ?, last_error = ?, updated_at = ?
		WHERE tenant_id = ? AND id = ?`,
		string(status), lastError, time.Now().UTC().Truncate(time.Microsecond).Format(timeLayout), tenantID, id)
	if err != nil {
		return fmt.Errorf("atualizar status: %w", err)
	}
	return ensureAffected(res)
}

func (r *Repo) Delete(ctx context.Context, tenantID, id string) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM sessions WHERE tenant_id = ? AND id = ?`, tenantID, id)
	if err != nil {
		return fmt.Errorf("remover sessão: %w", err)
	}
	return ensureAffected(res)
}

// CountByStatus alimenta o /health com o retrato das sessões.
func (r *Repo) CountByStatus(ctx context.Context) (map[string]int, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM sessions GROUP BY status`)
	if err != nil {
		return nil, fmt.Errorf("contar sessões: %w", err)
	}
	defer rows.Close()

	counts := make(map[string]int)
	for rows.Next() {
		var status string
		var total int
		if err := rows.Scan(&status, &total); err != nil {
			return nil, fmt.Errorf("ler contagem: %w", err)
		}
		counts[status] = total
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("percorrer contagem: %w", err)
	}
	return counts, nil
}

func ensureAffected(res sql.Result) error {
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("ler linhas afetadas: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanSession(row scanner) (Session, error) {
	var (
		s           Session
		status      string
		connectedAt sql.NullString
		createdAt   string
		updatedAt   string
	)
	if err := row.Scan(&s.ID, &s.TenantID, &s.OwnerUserID, &s.Name, &s.Phone, &status,
		&s.LastError, &connectedAt, &createdAt, &updatedAt); err != nil {
		return Session{}, err
	}

	s.Status = Status(status)
	if connectedAt.Valid {
		if parsed, err := time.Parse(timeLayout, connectedAt.String); err == nil {
			s.ConnectedAt = &parsed
		}
	}
	if parsed, err := time.Parse(timeLayout, createdAt); err == nil {
		s.CreatedAt = parsed
	}
	if parsed, err := time.Parse(timeLayout, updatedAt); err == nil {
		s.UpdatedAt = parsed
	}
	return s, nil
}
