package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// tempDir evita t.TempDir(): no Windows o arquivo do SQLite pode continuar
// preso por alguns ms após o Close e a limpeza falharia o teste por um motivo
// que não é do código (mesmo helper dos outros pacotes).
func tempDir(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("", "squamata-store-")
	if err != nil {
		t.Fatalf("criar diretório temporário: %v", err)
	}
	t.Cleanup(func() {
		for attempt := 0; attempt < 5; attempt++ {
			if err := os.RemoveAll(dir); err == nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	})
	return dir
}

// Teste de aceite da F0 (ADR 005): valida em Windows e Linux, sem CGO, que o
// par sqlstore + modernc.org/sqlite abre, migra, aplica WAL/foreign_keys e que
// o mesmo arquivo sobrevive ao fechamento/reabertura do processo.
func TestOpenMigratesAndPersists(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(tempDir(t), "nested", "whatsapp.db")

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Health(ctx); err != nil {
		t.Fatalf("Health: %v", err)
	}

	var journal, foreignKeys string
	if err := db.SQL.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil {
		t.Fatalf("pragma journal_mode: %v", err)
	}
	if err := db.SQL.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatalf("pragma foreign_keys: %v", err)
	}
	if journal != "wal" {
		t.Errorf("journal_mode = %q, esperado wal", journal)
	}
	if foreignKeys != "1" {
		t.Errorf("foreign_keys = %q, esperado 1", foreignKeys)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	again, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open apos fechar (persistencia): %v", err)
	}
	defer again.Close()

	if err := again.Health(ctx); err != nil {
		t.Fatalf("Health apos reabrir: %v", err)
	}
}
