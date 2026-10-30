package labels

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Repo espelha no SQLite as etiquetas do app state do WhatsApp.
//
// O whatsmeow não expõe leitura de labels: só patches (appstate.BuildLabelEdit)
// e eventos (LabelEdit, LabelAssociationChat). Este espelho é alimentado pelas
// escritas que fazemos e pelos eventos que chegam do celular.
type Repo struct {
	db *sql.DB
}

const timeLayout = time.RFC3339Nano

type Label struct {
	LabelID   string
	Name      string
	Color     int32
	Deleted   bool
	UpdatedAt time.Time
}

func NewRepo(db *sql.DB) *Repo {
	return &Repo{db: db}
}

// EnsureSchema usa CREATE TABLE IF NOT EXISTS para não disputar o
// PRAGMA user_version, que pertence ao pacote sessions.
func (r *Repo) EnsureSchema(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS wa_labels (
			session_id TEXT NOT NULL,
			label_id   TEXT NOT NULL,
			name       TEXT NOT NULL DEFAULT '',
			color      INTEGER NOT NULL DEFAULT 0,
			deleted    INTEGER NOT NULL DEFAULT 0,
			updated_at TEXT NOT NULL,
			PRIMARY KEY (session_id, label_id)
		)`,
		`CREATE TABLE IF NOT EXISTS wa_label_chats (
			session_id TEXT NOT NULL,
			label_id   TEXT NOT NULL,
			chat_jid   TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			PRIMARY KEY (session_id, label_id, chat_jid)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_wa_label_chats_label ON wa_label_chats (session_id, label_id)`,
	}
	for _, stmt := range statements {
		if _, err := r.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("criar schema de labels: %w", err)
		}
	}
	return nil
}

func (r *Repo) Upsert(ctx context.Context, sessionID string, label Label) error {
	if label.UpdatedAt.IsZero() {
		label.UpdatedAt = time.Now().UTC()
	}

	deleted := 0
	if label.Deleted {
		deleted = 1
	}

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO wa_labels (session_id, label_id, name, color, deleted, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (session_id, label_id) DO UPDATE SET
			name = excluded.name,
			color = excluded.color,
			deleted = excluded.deleted,
			updated_at = excluded.updated_at`,
		sessionID, label.LabelID, label.Name, label.Color, deleted, label.UpdatedAt.Format(timeLayout))
	if err != nil {
		return fmt.Errorf("gravar label: %w", err)
	}
	return nil
}

func (r *Repo) Delete(ctx context.Context, sessionID, labelID string) error {
	if _, err := r.db.ExecContext(ctx,
		`DELETE FROM wa_label_chats WHERE session_id = ? AND label_id = ?`, sessionID, labelID); err != nil {
		return fmt.Errorf("remover vínculos da label: %w", err)
	}
	if _, err := r.db.ExecContext(ctx,
		`DELETE FROM wa_labels WHERE session_id = ? AND label_id = ?`, sessionID, labelID); err != nil {
		return fmt.Errorf("remover label: %w", err)
	}
	return nil
}

// List devolve as labels vivas (as marcadas como deleted não aparecem).
func (r *Repo) List(ctx context.Context, sessionID string) ([]Label, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT label_id, name, color, deleted, updated_at
		FROM wa_labels WHERE session_id = ? AND deleted = 0
		ORDER BY name`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("listar labels: %w", err)
	}
	defer rows.Close()

	list := make([]Label, 0)
	for rows.Next() {
		var (
			l         Label
			deleted   int
			updatedAt string
		)
		if err := rows.Scan(&l.LabelID, &l.Name, &l.Color, &deleted, &updatedAt); err != nil {
			return nil, fmt.Errorf("ler label: %w", err)
		}
		l.Deleted = deleted == 1
		if parsed, err := time.Parse(timeLayout, updatedAt); err == nil {
			l.UpdatedAt = parsed
		}
		list = append(list, l)
	}
	return list, rows.Err()
}

func (r *Repo) SetChatLabel(ctx context.Context, sessionID, labelID, chatJID string, labeled bool) error {
	if !labeled {
		if _, err := r.db.ExecContext(ctx, `
			DELETE FROM wa_label_chats
			WHERE session_id = ? AND label_id = ? AND chat_jid = ?`,
			sessionID, labelID, chatJID); err != nil {
			return fmt.Errorf("desvincular label: %w", err)
		}
		return nil
	}

	now := time.Now().UTC().Format(timeLayout)
	if _, err := r.db.ExecContext(ctx, `
		INSERT INTO wa_label_chats (session_id, label_id, chat_jid, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (session_id, label_id, chat_jid) DO UPDATE SET updated_at = excluded.updated_at`,
		sessionID, labelID, chatJID, now); err != nil {
		return fmt.Errorf("vincular label: %w", err)
	}
	return nil
}

// ChatsByLabel devolve os JIDs que carregam a label.
func (r *Repo) ChatsByLabel(ctx context.Context, sessionID, labelID string) ([]string, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT chat_jid FROM wa_label_chats WHERE session_id = ? AND label_id = ? ORDER BY chat_jid`,
		sessionID, labelID)
	if err != nil {
		return nil, fmt.Errorf("listar chats da label: %w", err)
	}
	defer rows.Close()

	chats := make([]string, 0)
	for rows.Next() {
		var jid string
		if err := rows.Scan(&jid); err != nil {
			return nil, fmt.Errorf("ler chat da label: %w", err)
		}
		chats = append(chats, jid)
	}
	return chats, rows.Err()
}

// LabelsByChat devolve as labels de um chat.
func (r *Repo) LabelsByChat(ctx context.Context, sessionID, chatJID string) ([]string, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT label_id FROM wa_label_chats WHERE session_id = ? AND chat_jid = ? ORDER BY label_id`,
		sessionID, chatJID)
	if err != nil {
		return nil, fmt.Errorf("listar labels do chat: %w", err)
	}
	defer rows.Close()

	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("ler label do chat: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
