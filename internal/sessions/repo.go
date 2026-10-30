package sessions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	// schemaVersion é a última migração conhecida. Cada entrada de migrations
	// leva o banco de (versão-1) para (versão).
	schemaVersion = 3
	// RFC3339Nano preserva ordem lexicográfica e mantém o fuso explícito.
	timeLayout = time.RFC3339Nano
)

// migrations é aplicada em ordem, com PRAGMA user_version como controle.
// A v1 criou a tabela; a v2 acrescentou device_jid, que é o vínculo com o
// registro do sqlstore (o device só ganha JID depois do pareamento).
var migrations = map[int][]string{
	1: {
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
	},
	2: {
		`ALTER TABLE sessions ADD COLUMN device_jid TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_device_jid ON sessions (device_jid)`,
	},
	3: {
		`ALTER TABLE sessions ADD COLUMN receive_groups INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE sessions ADD COLUMN receive_newsletters INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE sessions ADD COLUMN receive_broadcast INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE sessions ADD COLUMN media_profile TEXT NOT NULL DEFAULT 'chat'`,
		`ALTER TABLE sessions ADD COLUMN max_media_bytes INTEGER NOT NULL DEFAULT 8388608`,
	},
}

// Repo guarda os metadados das sessões no mesmo arquivo SQLite do sqlstore.
type Repo struct {
	db *sql.DB
}

func NewRepo(db *sql.DB) *Repo {
	return &Repo{db: db}
}

// Migrate aplica as migrações pendentes usando PRAGMA user_version, que é o
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

	for version := current + 1; version <= schemaVersion; version++ {
		statements, ok := migrations[version]
		if !ok {
			return fmt.Errorf("migração %d não definida", version)
		}
		if err := r.applyMigration(ctx, version, statements); err != nil {
			return err
		}
	}
	return nil
}

func (r *Repo) applyMigration(ctx context.Context, version int, statements []string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("abrir transação da migração %d: %w", version, err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, stmt := range statements {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migração %d: %w", version, err)
		}
	}
	// PRAGMA não aceita placeholder; o valor vem de uma constante do pacote.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		return fmt.Errorf("gravar user_version %d: %w", version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("confirmar migração %d: %w", version, err)
	}
	return nil
}

func (r *Repo) Create(ctx context.Context, in Session) (Session, error) {
	if err := in.Normalize(); err != nil {
		return Session{}, err
	}

	policy := in.Policy()
	if err := policy.Normalize(); err != nil {
		return Session{}, err
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	s := in
	s.ID = uuid.NewString()
	s.Status = StatusDisconnected
	s.LastError = ""
	s.DeviceJID = ""
	s.ConnectedAt = nil
	s.CreatedAt = now
	s.UpdatedAt = now
	s.ReceiveGroups = policy.ReceiveGroups
	s.ReceiveNewsletters = policy.ReceiveNewsletters
	s.ReceiveBroadcast = policy.ReceiveBroadcast
	s.MediaProfile = policy.MediaProfile
	s.MaxMediaBytes = policy.MaxMediaBytes

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO sessions (id, tenant_id, owner_user_id, name, phone, status,
			last_error, connected_at, created_at, updated_at, device_jid,
			receive_groups, receive_newsletters, receive_broadcast, media_profile, max_media_bytes)
		VALUES (?, ?, ?, ?, ?, ?, ?, NULL, ?, ?, '', ?, ?, ?, ?, ?)`,
		s.ID, s.TenantID, s.OwnerUserID, s.Name, s.Phone, string(s.Status), s.LastError,
		s.CreatedAt.Format(timeLayout), s.UpdatedAt.Format(timeLayout),
		boolToInt(s.ReceiveGroups), boolToInt(s.ReceiveNewsletters), boolToInt(s.ReceiveBroadcast),
		s.MediaProfile, s.MaxMediaBytes,
	)
	if err != nil {
		return Session{}, fmt.Errorf("inserir sessão: %w", err)
	}
	return s, nil
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// Get sempre filtra por tenant: um sessionId de outro tenant não resolve.
func (r *Repo) Get(ctx context.Context, tenantID, id string) (Session, error) {
	row := r.db.QueryRowContext(ctx, selectColumns+` WHERE tenant_id = ? AND id = ?`, tenantID, id)

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
	return r.query(ctx, selectColumns+` WHERE tenant_id = ? ORDER BY created_at`, tenantID)
}

// ListPaired devolve todas as sessões já pareadas, de todos os tenants. É o que
// a reconexão no boot precisa; não é rota HTTP, por isso não filtra tenant.
func (r *Repo) ListPaired(ctx context.Context) ([]Session, error) {
	return r.query(ctx, selectColumns+` WHERE device_jid <> '' ORDER BY created_at`)
}

func (r *Repo) query(ctx context.Context, query string, args ...any) ([]Session, error) {
	rows, err := r.db.QueryContext(ctx, query, args...)
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

// RuntimePatch atualiza só os campos informados. A máquina de estados mexe em
// pedaços diferentes conforme o evento do WhatsApp, então um UPDATE parcial
// evita sobrescrever o que outro handler acabou de gravar.
type RuntimePatch struct {
	Status      *Status
	LastError   *string
	Phone       *string
	DeviceJID   *string
	ConnectedAt *time.Time
	ClearConnAt bool
}

func (r *Repo) UpdateRuntime(ctx context.Context, tenantID, id string, patch RuntimePatch) error {
	if patch.Status != nil && !patch.Status.Valid() {
		return ErrInvalidStatus
	}

	sets := make([]string, 0, 6)
	args := make([]any, 0, 8)

	if patch.Status != nil {
		sets = append(sets, "status = ?")
		args = append(args, string(*patch.Status))
	}
	if patch.LastError != nil {
		sets = append(sets, "last_error = ?")
		args = append(args, *patch.LastError)
	}
	if patch.Phone != nil {
		sets = append(sets, "phone = ?")
		args = append(args, *patch.Phone)
	}
	if patch.DeviceJID != nil {
		sets = append(sets, "device_jid = ?")
		args = append(args, *patch.DeviceJID)
	}
	if patch.ConnectedAt != nil {
		sets = append(sets, "connected_at = ?")
		args = append(args, patch.ConnectedAt.UTC().Format(timeLayout))
	} else if patch.ClearConnAt {
		sets = append(sets, "connected_at = NULL")
	}
	if len(sets) == 0 {
		return nil
	}

	sets = append(sets, "updated_at = ?")
	args = append(args, time.Now().UTC().Truncate(time.Microsecond).Format(timeLayout))
	args = append(args, tenantID, id)

	// #nosec G201 -- os fragmentos vêm de constantes do pacote, nunca do usuário.
	query := "UPDATE sessions SET " + strings.Join(sets, ", ") + " WHERE tenant_id = ? AND id = ?"
	res, err := r.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("atualizar sessão: %w", err)
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

const selectColumns = `
	SELECT id, tenant_id, owner_user_id, name, phone, status, last_error,
		connected_at, created_at, updated_at, device_jid,
		receive_groups, receive_newsletters, receive_broadcast, media_profile, max_media_bytes
	FROM sessions`

type scanner interface {
	Scan(dest ...any) error
}

func scanSession(row scanner) (Session, error) {
	var (
		s                              Session
		status                         string
		connectedAt                    sql.NullString
		createdAt                      string
		updatedAt                      string
		groups, newsletters, broadcast int
	)
	if err := row.Scan(&s.ID, &s.TenantID, &s.OwnerUserID, &s.Name, &s.Phone, &status,
		&s.LastError, &connectedAt, &createdAt, &updatedAt, &s.DeviceJID,
		&groups, &newsletters, &broadcast, &s.MediaProfile, &s.MaxMediaBytes); err != nil {
		return Session{}, err
	}

	s.Status = Status(status)
	s.ReceiveGroups = groups == 1
	s.ReceiveNewsletters = newsletters == 1
	s.ReceiveBroadcast = broadcast == 1
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
