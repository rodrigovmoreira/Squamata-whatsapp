package sessions

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// tempDir evita o t.TempDir(): no Windows o arquivo do SQLite pode continuar
// preso por alguns milissegundos após o Close, e a limpeza do t.TempDir falha
// o teste ("A pasta não está vazia") por um motivo que não é do código.
func tempDir(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("", "squamata-sessions-")
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

	path := filepath.Join(tempDir(t), "sessions.db")
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

func TestUpdateRuntime(t *testing.T) {
	repo, _ := newTestRepo(t)
	ctx := context.Background()

	created, err := repo.Create(ctx, Session{TenantID: "tenant-a", Name: "Vendas"})
	if err != nil {
		t.Fatalf("criar: %v", err)
	}

	ready := StatusReady
	phone := "5511999999999"
	if err := repo.UpdateRuntime(ctx, "tenant-a", created.ID, RuntimePatch{Status: &ready, Phone: &phone}); err != nil {
		t.Fatalf("UpdateRuntime: %v", err)
	}
	got, err := repo.Get(ctx, "tenant-a", created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusReady {
		t.Errorf("status = %q, esperado %q", got.Status, StatusReady)
	}
	if got.Phone != phone {
		t.Errorf("phone = %q, esperado %q", got.Phone, phone)
	}

	invalid := Status("inventado")
	if err := repo.UpdateRuntime(ctx, "tenant-a", created.ID, RuntimePatch{Status: &invalid}); !errors.Is(err, ErrInvalidStatus) {
		t.Errorf("status inválido: erro = %v, esperado ErrInvalidStatus", err)
	}
	if err := repo.UpdateRuntime(ctx, "tenant-a", "nao-existe", RuntimePatch{Status: &ready}); !errors.Is(err, ErrNotFound) {
		t.Errorf("sessão inexistente: erro = %v, esperado ErrNotFound", err)
	}
}

func TestUpdateRuntimePartialPatchKeepsOtherFields(t *testing.T) {
	repo, _ := newTestRepo(t)
	ctx := context.Background()

	created, err := repo.Create(ctx, Session{TenantID: "tenant-a", Name: "Vendas"})
	if err != nil {
		t.Fatalf("criar: %v", err)
	}

	jid := "5511999999999@s.whatsapp.net"
	ready := StatusReady
	if err := repo.UpdateRuntime(ctx, "tenant-a", created.ID, RuntimePatch{DeviceJID: &jid, Status: &ready}); err != nil {
		t.Fatalf("primeiro patch: %v", err)
	}

	// Segundo patch mexe só no erro e não pode apagar o device_jid nem o status.
	empty := ""
	if err := repo.UpdateRuntime(ctx, "tenant-a", created.ID, RuntimePatch{LastError: &empty}); err != nil {
		t.Fatalf("segundo patch: %v", err)
	}

	got, err := repo.Get(ctx, "tenant-a", created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.DeviceJID != jid {
		t.Errorf("device_jid = %q, esperado %q", got.DeviceJID, jid)
	}
	if got.Status != StatusReady {
		t.Errorf("status = %q, esperado %q", got.Status, StatusReady)
	}
}

func TestListPaired(t *testing.T) {
	repo, _ := newTestRepo(t)
	ctx := context.Background()

	paired, err := repo.Create(ctx, Session{TenantID: "tenant-a", Name: "Pareada"})
	if err != nil {
		t.Fatalf("criar pareada: %v", err)
	}
	if _, err := repo.Create(ctx, Session{TenantID: "tenant-b", Name: "Não pareada"}); err != nil {
		t.Fatalf("criar não pareada: %v", err)
	}

	jid := "5511888888888@s.whatsapp.net"
	if err := repo.UpdateRuntime(ctx, "tenant-a", paired.ID, RuntimePatch{DeviceJID: &jid}); err != nil {
		t.Fatalf("marcar device_jid: %v", err)
	}

	list, err := repo.ListPaired(ctx)
	if err != nil {
		t.Fatalf("ListPaired: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListPaired = %d sessões, esperado 1", len(list))
	}
	if list[0].ID != paired.ID {
		t.Errorf("id = %q, esperado %q", list[0].ID, paired.ID)
	}
}

// TestMigrationFromV1 cobre o banco que já existe em dev: ele foi criado na
// v1 (sem device_jid) e precisa ganhar a coluna sem perder dados.
func TestMigrationFromV1(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(tempDir(t), "v1.db")
	db := openTestDB(t, path)

	for _, stmt := range migrations[1] {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("aplicar schema v1: %v", err)
		}
	}
	if _, err := db.ExecContext(ctx, "PRAGMA user_version = 1"); err != nil {
		t.Fatalf("marcar user_version: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO sessions (id, tenant_id, owner_user_id, name, phone, status,
			last_error, created_at, updated_at)
		VALUES ('antiga', 'tenant-a', '', 'Vendas', '', 'disconnected', '',
			'2026-10-29T10:00:00Z', '2026-10-29T10:00:00Z')`); err != nil {
		t.Fatalf("inserir linha da v1: %v", err)
	}

	repo := NewRepo(db)
	if err := repo.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	got, err := repo.Get(ctx, "tenant-a", "antiga")
	if err != nil {
		t.Fatalf("Get após migração: %v", err)
	}
	if got.Name != "Vendas" {
		t.Errorf("dado da v1 se perdeu: name = %q", got.Name)
	}
	if got.DeviceJID != "" {
		t.Errorf("device_jid = %q, esperado vazio", got.DeviceJID)
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
	path := filepath.Join(tempDir(t), "sessions.db")
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
