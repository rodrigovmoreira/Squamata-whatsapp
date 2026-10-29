package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	"go.mau.fi/whatsmeow/store/sqlstore"
	waLog "go.mau.fi/whatsmeow/util/log"

	// Driver SQLite em Go puro (sem CGO): mantem a imagem final minima e
	// permite base distroless. O rotulo de dialeto passado ao sqlstore
	// continua sendo "sqlite3", que e o que o dbutil entende (ADR 005).
	_ "modernc.org/sqlite"
)

// DB agrupa o handle SQL e o container do whatsmeow. Os dois compartilham o
// mesmo arquivo: o container gerencia as tabelas dele (prefixo whatsmeow_) e o
// Calango-bot so conhece o sessionId opaco devolvido por este servico.
type DB struct {
	SQL       *sql.DB
	Container *sqlstore.Container
}

// Open abre (criando se preciso) o arquivo SQLite e aplica as migracoes do
// whatsmeow. O DSN precisa de foreign_keys=ON: sqlstore.Container.Upgrade
// recusa prosseguir com "foreign keys are not enabled".
func Open(ctx context.Context, path string) (*DB, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("criar diretorio do banco %q: %w", dir, err)
		}
	}

	dsn := fmt.Sprintf(
		"file:%s?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)",
		filepath.ToSlash(path),
	)

	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("abrir sqlite: %w", err)
	}

	// SQLite tem um unico escritor. Serializar no pool evita
	// "database is locked" sem depender de retry; o volume deste servico e
	// baixo (pareamento e envio ocasionais).
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)

	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("conectar ao sqlite: %w", err)
	}

	// NewWithDB (e nao New) porque o driver e "sqlite" (modernc), enquanto o
	// rotulo de dialeto que o dbutil reconhece e "sqlite3". Upgrade nao e
	// automatico neste caminho.
	container := sqlstore.NewWithDB(sqlDB, "sqlite3", waLog.Stdout("Database", "WARN", false))
	if err := container.Upgrade(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("aplicar migracoes do whatsmeow: %w", err)
	}

	return &DB{SQL: sqlDB, Container: container}, nil
}

// Health confirma que o banco responde E que as tabelas do whatsmeow existem.
// Um "SELECT 1" sozinho passaria mesmo com as migracoes nao aplicadas.
func (d *DB) Health(ctx context.Context) error {
	var one int
	if err := d.SQL.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		return fmt.Errorf("consulta simples: %w", err)
	}

	var devices int
	if err := d.SQL.QueryRowContext(ctx, "SELECT COUNT(*) FROM whatsmeow_device").Scan(&devices); err != nil {
		return fmt.Errorf("tabela whatsmeow_device: %w", err)
	}

	return nil
}

func (d *DB) Close() error {
	return d.SQL.Close()
}
