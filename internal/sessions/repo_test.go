package sessions

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func openTestDB(t *testing.T, path string) *sql.DB {
	t.Helper()

	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("abrir sqlite: %v", err)
	}
	// Mesma configuração do serviço: um escritor, sem "database is locked".
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = db.Close() })

	return db
}

func newTestRepo(t *testing.T) (*Repo, string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "sessions.db")
	repo := NewRepo(openTestDB(t, path))
	if err := repo.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return repo, path
}

func TestMigrateIsIdempotent(t *testing.T) {
	repo, _ := newTestRepo(t)
	ctx := context.Background()

	if err := repo.Migrate(ctx); err != nil {
		t.Fatalf("segunda migração deveria ser no-op, veio erro: %v", err)
	}

	var version int
	if err := repo.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("ler user_version: %v", err)
	}
	if version != schemaVersion {
		t.Errorf("user_version = %d, esperado %d", version, schemaVersion)
	}
}

func TestCreateGetAndList(t *testing.T) {
	repo, _ := newTestRepo(t)
	ctx := context.Background()

	first, err := repo.Create(ctx, Session{TenantID: "tenant-a", Name: "Vendas"})
	if err != nil {
		t.Fatalf("criar primeira: %v", err)
	}
	if first.ID == "" {
		t.Fatal("id não foi gerado")
	}
	if first.Status != StatusDisconnected {
		t.Errorf("status inicial = %q, esperado %q", first.Status, StatusDisconnected)
	}

	if _, err := repo.Create(ctx, Session{TenantID: "tenant-a", Name: "Suporte"}); err != nil {
		t.Fatalf("criar segunda: %v", err)
	}
	if _, err := repo.Create(ctx, Session{TenantID: "tenant-b", Name: "Outra empresa"}); err != nil {
		t.Fatalf("criar de outro tenant: %v", err)
	}

	got, err := repo.Get(ctx, "tenant-a", first.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "Vendas" {
		t.Errorf("name = %q, esperado Vendas", got.Name)
	}
	if got.CreatedAt.IsZero() {
		t.Error("createdAt não foi preenchido")
	}

	listA, err := repo.List(ctx, "tenant-a")
	if err != nil {
		t.Fatalf("List tenant-a: %v", err)
	}
	if len(listA) != 2 {
		t.Errorf("List tenant-a = %d sessões, esperado 2", len(listA))
	}

	listB, err := repo.List(ctx, "tenant-b")
	if err != nil {
		t.Fatalf("List tenant-b: %v", err)
	}
	if len(listB) != 1 {
		t.Errorf("List tenant-b = %d sessões, esperado 1", len(listB))
	}
}

func TestCreateValidatesInput(t *testing.T) {
	repo, _ := newTestRepo(t)
	ctx := context.Background()

	if _, err := repo.Create(ctx, Session{TenantID: "tenant-a"}); !errors.Is(err, ErrEmptyName) {
		t.Errorf("sem nome: erro = %v, esperado ErrEmptyName", err)
	}
	if _, err := repo.Create(ctx, Session{Name: "Vendas"}); !errors.Is(err, ErrEmptyTenant) {
		t.Errorf("sem tenant: erro = %v, esperado ErrEmptyTenant", err)
	}

	longName := make([]rune, maxNameLength+1)
	for i := range longName {
		longName[i] = 'a'
	}
	if _, err := repo.Create(ctx, Session{TenantID: "tenant-a", Name: string(longName)}); !errors.Is(err, ErrNameTooLong) {
		t.Errorf("nome longo: erro = %v, esperado ErrNameTooLong", err)
	}
}

func TestTenantIsolation(t *testing.T) {
	repo, _ := newTestRepo(t)
	ctx := context.Background()

	created, err := repo.Create(ctx, Session{TenantID: "tenant-a", Name: "Vendas"})
	if err != nil {
		t.Fatalf("criar: %v", err)
	}

	if _, err := repo.Get(ctx, "tenant-b", created.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get com outro tenant: erro = %v, esperado ErrNotFound", err)
	}
	if err := repo.Delete(ctx, "tenant-b", created.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete com outro tenant: erro = %v, esperado ErrNotFound", err)
	}
	if _, err := repo.Get(ctx, "tenant-a", created.ID); err != nil {
		t.Fatalf("a sessão original deveria continuar existindo: %v", err)
	}
}

func TestUpdateStatus(t *testing.T) {
	repo, _ := newTestRepo(t)
	ctx := context.Background()

	created, err := repo.Create(ctx, Session{TenantID: "tenant-a", Name: "Vendas"})
	if err != nil {
		t.Fatalf("criar: %v", err)
	}

	if err := repo.UpdateStatus(ctx, "tenant-a", created.ID, StatusReady, ""); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	got, err := repo.Get(ctx, "tenant-a", created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusReady {
		t.Errorf("status = %q, esperado %q", got.Status, StatusReady)
	}

	if err := repo.UpdateStatus(ctx, "tenant-a", created.ID, Status("inventado"), ""); !errors.Is(err, ErrInvalidStatus) {
		t.Errorf("status inválido: erro = %v, esperado ErrInvalidStatus", err)
	}
	if err := repo.UpdateStatus(ctx, "tenant-a", "nao-existe", StatusReady, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("sessão inexistente: erro = %v, esperado ErrNotFound", err)
	}
}

func TestDeleteAndCounts(t *testing.T) {
	repo, _ := newTestRepo(t)
	ctx := context.Background()

	created, err := repo.Create(ctx, Session{TenantID: "tenant-a", Name: "Vendas"})
	if err != nil {
		t.Fatalf("criar: %v", err)
	}
	if _, err := repo.Create(ctx, Session{TenantID: "tenant-a", Name: "Suporte"}); err != nil {
		t.Fatalf("criar segunda: %v", err)
	}

	counts, err := repo.CountByStatus(ctx)
	if err != nil {
		t.Fatalf("CountByStatus: %v", err)
	}
	if counts[string(StatusDisconnected)] != 2 {
		t.Errorf("contagem disconnected = %d, esperado 2", counts[string(StatusDisconnected)])
	}

	if err := repo.Delete(ctx, "tenant-a", created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := repo.Get(ctx, "tenant-a", created.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("após Delete: erro = %v, esperado ErrNotFound", err)
	}
	if err := repo.Delete(ctx, "tenant-a", created.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete repetido: erro = %v, esperado ErrNotFound", err)
	}
}

// TestPersistenceAcrossReopen é o aceite da F1: a sessão sobrevive ao restart
// porque o SQLite está em arquivo, não em memória.
func TestPersistenceAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	ctx := context.Background()

	db := openTestDB(t, path)
	first := NewRepo(db)
	if err := first.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	created, err := first.Create(ctx, Session{TenantID: "tenant-a", Name: "Vendas"})
	if err != nil {
		t.Fatalf("criar: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("fechar banco: %v", err)
	}

	reopened := NewRepo(openTestDB(t, path))
	if err := reopened.Migrate(ctx); err != nil {
		t.Fatalf("Migrate na reabertura: %v", err)
	}
	got, err := reopened.Get(ctx, "tenant-a", created.ID)
	if err != nil {
		t.Fatalf("Get após reabrir: %v", err)
	}
	if got.Name != "Vendas" {
		t.Errorf("name = %q, esperado Vendas", got.Name)
	}
}
