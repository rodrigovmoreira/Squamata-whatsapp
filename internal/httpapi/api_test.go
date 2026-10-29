package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/rodrigovmoreira/Squamata-whatsapp/internal/sessions"

	_ "modernc.org/sqlite"
)

const testToken = "token-de-teste"

type fakeHealth struct{ err error }

func (f fakeHealth) Health(context.Context) error { return f.err }

func newTestHandler(t *testing.T, health HealthChecker) http.Handler {
	t.Helper()

	path := filepath.Join(t.TempDir(), "api.db")
	dsn := "file:" + filepath.ToSlash(path) + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("abrir sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	repo := sessions.NewRepo(db)
	if err := repo.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(repo, health, testToken, logger).Handler()
}

func do(t *testing.T, handler http.Handler, method, target, token, body string) *httptest.ResponseRecorder {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = bytes.NewBufferString(body)
	}
	req := httptest.NewRequest(method, target, reader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()

	var payload T
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("resposta não é JSON válido (%q): %v", rec.Body.String(), err)
	}
	return payload
}

func TestHealthIsPublicAndReportsStore(t *testing.T) {
	handler := newTestHandler(t, fakeHealth{})

	rec := do(t, handler, http.MethodGet, "/api/v1/health", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, esperado 200 (health não exige token)", rec.Code)
	}

	body := decode[healthResponse](t, rec)
	if body.Status != "UP" {
		t.Errorf("status = %q, esperado UP", body.Status)
	}
	if body.Service != "squamata-whatsapp" {
		t.Errorf("service = %q", body.Service)
	}
	if !body.Store.OK {
		t.Error("store.ok deveria ser true")
	}
	if body.Sessions.ByStatus == nil {
		t.Error("sessions.byStatus não deveria ser nil")
	}
}

func TestHealthReturns503WhenStoreFails(t *testing.T) {
	handler := newTestHandler(t, fakeHealth{err: errors.New("arquivo inacessível")})

	rec := do(t, handler, http.MethodGet, "/api/v1/health", "", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, esperado 503", rec.Code)
	}
	body := decode[healthResponse](t, rec)
	if body.Status != "DOWN" || body.Store.OK {
		t.Errorf("esperado status DOWN e store.ok=false, veio %+v", body)
	}
}

func TestProtectedRoutesRequireToken(t *testing.T) {
	handler := newTestHandler(t, fakeHealth{})
	route := "/api/v1/tenants/tenant-a/sessions"

	if rec := do(t, handler, http.MethodGet, route, "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("sem token: status = %d, esperado 401", rec.Code)
	}
	if rec := do(t, handler, http.MethodGet, route, "token-errado", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("token errado: status = %d, esperado 401", rec.Code)
	}

	rec := do(t, handler, http.MethodGet, route, testToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("token correto: status = %d, esperado 200", rec.Code)
	}
	if list := decode[[]sessionResponse](t, rec); len(list) != 0 {
		t.Errorf("lista inicial = %d itens, esperado 0", len(list))
	}
}

func TestSessionLifecycle(t *testing.T) {
	handler := newTestHandler(t, fakeHealth{})
	collection := "/api/v1/tenants/tenant-a/sessions"

	created := do(t, handler, http.MethodPost, collection, testToken, `{"name":"Vendas","ownerUserId":"user-1"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("criar: status = %d, esperado 201 (%s)", created.Code, created.Body.String())
	}
	session := decode[sessionResponse](t, created)
	if session.ID == "" {
		t.Fatal("id não veio na resposta")
	}
	if session.Status != string(sessions.StatusDisconnected) {
		t.Errorf("status = %q, esperado disconnected", session.Status)
	}
	if session.OwnerUserID != "user-1" {
		t.Errorf("ownerUserId = %q", session.OwnerUserID)
	}

	item := collection + "/" + session.ID
	if rec := do(t, handler, http.MethodGet, item, testToken, ""); rec.Code != http.StatusOK {
		t.Fatalf("buscar: status = %d, esperado 200", rec.Code)
	}

	listRec := do(t, handler, http.MethodGet, collection, testToken, "")
	if list := decode[[]sessionResponse](t, listRec); len(list) != 1 {
		t.Errorf("lista = %d itens, esperado 1", len(list))
	}

	if rec := do(t, handler, http.MethodDelete, item, testToken, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("remover: status = %d, esperado 204", rec.Code)
	}

	gone := do(t, handler, http.MethodGet, item, testToken, "")
	if gone.Code != http.StatusNotFound {
		t.Fatalf("após remover: status = %d, esperado 404", gone.Code)
	}
	if body := decode[errorBody](t, gone); body.Error.Code != "SESSION_NOT_FOUND" {
		t.Errorf("code = %q, esperado SESSION_NOT_FOUND", body.Error.Code)
	}
}

func TestTenantIsolationOverHTTP(t *testing.T) {
	handler := newTestHandler(t, fakeHealth{})

	created := do(t, handler, http.MethodPost, "/api/v1/tenants/tenant-a/sessions", testToken, `{"name":"Vendas"}`)
	session := decode[sessionResponse](t, created)

	rec := do(t, handler, http.MethodGet, "/api/v1/tenants/tenant-b/sessions/"+session.ID, testToken, "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("sessão de outro tenant: status = %d, esperado 404", rec.Code)
	}
}

func TestCreateSessionValidation(t *testing.T) {
	handler := newTestHandler(t, fakeHealth{})
	collection := "/api/v1/tenants/tenant-a/sessions"

	cases := map[string]string{
		"sem nome":    `{"ownerUserId":"user-1"}`,
		"json torto":  `{"name":`,
		"corpo vazio": ``,
	}
	for name, body := range cases {
		rec := do(t, handler, http.MethodPost, collection, testToken, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, esperado 400", name, rec.Code)
		}
	}
}
