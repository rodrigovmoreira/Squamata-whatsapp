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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rodrigovmoreira/Squamata-whatsapp/internal/sessions"
	"github.com/rodrigovmoreira/Squamata-whatsapp/internal/wa"

	_ "modernc.org/sqlite"
)

const testToken = "token-de-teste"

type fakeHealth struct{ err error }

func (f fakeHealth) Health(context.Context) error { return f.err }

// tempDir evita t.TempDir(): no Windows o SQLite pode continuar segurando o
// arquivo por alguns ms apÃ³s o Close e a limpeza falharia o teste.
func tempDir(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("", "squamata-httpapi-")
	if err != nil {
		t.Fatalf("criar diretÃ³rio temporÃ¡rio: %v", err)
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

// fakeManager registra as chamadas e delega o que Ã© persistÃªncia ao repositÃ³rio
// real, para o teste de HTTP nÃ£o precisar de whatsmeow nem de rede.
type fakeManager struct {
	repo         *sessions.Repo
	qr           wa.QRCode
	qrErr        error
	connectErr   error
	sendTextErr  error
	sendMediaErr error
	presenceErr  error
	lastText     sendTextRequest
	lastMedia    sendMediaRequest
	presence     wa.PresenceKind
	calls        []string
}

func (f *fakeManager) Connect(ctx context.Context, tenantID, sessionID string) (sessions.Session, error) {
	f.calls = append(f.calls, "connect")
	if f.connectErr != nil {
		return sessions.Session{}, f.connectErr
	}
	return f.repo.Get(ctx, tenantID, sessionID)
}

func (f *fakeManager) Disconnect(ctx context.Context, tenantID, sessionID string) (sessions.Session, error) {
	f.calls = append(f.calls, "disconnect")
	return f.repo.Get(ctx, tenantID, sessionID)
}

func (f *fakeManager) Delete(ctx context.Context, tenantID, sessionID string) error {
	f.calls = append(f.calls, "delete")
	return f.repo.Delete(ctx, tenantID, sessionID)
}

func (f *fakeManager) QRCode(tenantID, sessionID string) (wa.QRCode, error) {
	f.calls = append(f.calls, "qr")
	if f.qrErr != nil {
		return wa.QRCode{}, f.qrErr
	}
	return f.qr, nil
}

func (f *fakeManager) SendText(_ context.Context, _, _ string, to, text string) error {
	f.calls = append(f.calls, "sendText")
	f.lastText = sendTextRequest{To: to, Text: text}
	return f.sendTextErr
}

func (f *fakeManager) SendMedia(_ context.Context, _, _ string, to, mediaURL, mime, caption string) error {
	f.calls = append(f.calls, "sendMedia")
	f.lastMedia = sendMediaRequest{To: to, MediaURL: mediaURL, Mime: mime, Caption: caption}
	return f.sendMediaErr
}

func (f *fakeManager) SendPresence(_ context.Context, _, _ string, _ string, kind wa.PresenceKind) error {
	f.calls = append(f.calls, "presence")
	f.presence = kind
	return f.presenceErr
}

func newTestHandler(t *testing.T, health HealthChecker) (http.Handler, *fakeManager) {
	t.Helper()

	path := filepath.Join(tempDir(t), "api.db")
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

	manager := &fakeManager{repo: repo}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(repo, health, manager, testToken, logger).Handler(), manager
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
		t.Fatalf("resposta nÃ£o Ã© JSON vÃ¡lido (%q): %v", rec.Body.String(), err)
	}
	return payload
}

func TestHealthIsPublicAndReportsStore(t *testing.T) {
	handler, _ := newTestHandler(t, fakeHealth{})

	rec := do(t, handler, http.MethodGet, "/api/v1/health", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, esperado 200 (health nÃ£o exige token)", rec.Code)
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
		t.Error("sessions.byStatus nÃ£o deveria ser nil")
	}
}

func TestHealthReturns503WhenStoreFails(t *testing.T) {
	handler, _ := newTestHandler(t, fakeHealth{err: errors.New("arquivo inacessÃ­vel")})

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
	handler, _ := newTestHandler(t, fakeHealth{})
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
	handler, manager := newTestHandler(t, fakeHealth{})
	collection := "/api/v1/tenants/tenant-a/sessions"

	created := do(t, handler, http.MethodPost, collection, testToken, `{"name":"Vendas","ownerUserId":"user-1"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("criar: status = %d, esperado 201 (%s)", created.Code, created.Body.String())
	}
	session := decode[sessionResponse](t, created)
	if session.ID == "" {
		t.Fatal("id nÃ£o veio na resposta")
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
	if len(manager.calls) != 1 || manager.calls[0] != "delete" {
		t.Errorf("DELETE deveria passar pelo gerenciador, chamadas = %v", manager.calls)
	}

	gone := do(t, handler, http.MethodGet, item, testToken, "")
	if gone.Code != http.StatusNotFound {
		t.Fatalf("apÃ³s remover: status = %d, esperado 404", gone.Code)
	}
	if body := decode[errorBody](t, gone); body.Error.Code != "SESSION_NOT_FOUND" {
		t.Errorf("code = %q, esperado SESSION_NOT_FOUND", body.Error.Code)
	}
}

func TestConnectAndDisconnectRoutes(t *testing.T) {
	handler, manager := newTestHandler(t, fakeHealth{})
	collection := "/api/v1/tenants/tenant-a/sessions"

	created := do(t, handler, http.MethodPost, collection, testToken, `{"name":"Vendas"}`)
	session := decode[sessionResponse](t, created)
	item := collection + "/" + session.ID

	if rec := do(t, handler, http.MethodPost, item+"/connect", testToken, ""); rec.Code != http.StatusOK {
		t.Fatalf("connect: status = %d, esperado 200 (%s)", rec.Code, rec.Body.String())
	}
	if rec := do(t, handler, http.MethodPost, item+"/disconnect", testToken, ""); rec.Code != http.StatusOK {
		t.Fatalf("disconnect: status = %d, esperado 200", rec.Code)
	}
	if len(manager.calls) != 2 || manager.calls[0] != "connect" || manager.calls[1] != "disconnect" {
		t.Errorf("chamadas = %v, esperado [connect disconnect]", manager.calls)
	}

	if rec := do(t, handler, http.MethodPost, collection+"/nao-existe/connect", testToken, ""); rec.Code != http.StatusNotFound {
		t.Errorf("connect em sessÃ£o inexistente: status = %d, esperado 404", rec.Code)
	}
}

func TestConnectReturnsBadGatewayWhenWhatsAppFails(t *testing.T) {
	handler, manager := newTestHandler(t, fakeHealth{})
	manager.connectErr = errors.New("rede fora")

	created := do(t, handler, http.MethodPost, "/api/v1/tenants/tenant-a/sessions", testToken, `{"name":"Vendas"}`)
	session := decode[sessionResponse](t, created)

	rec := do(t, handler, http.MethodPost, "/api/v1/tenants/tenant-a/sessions/"+session.ID+"/connect", testToken, "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, esperado 502", rec.Code)
	}
	if body := decode[errorBody](t, rec); body.Error.Code != "WHATSAPP_UNAVAILABLE" {
		t.Errorf("code = %q, esperado WHATSAPP_UNAVAILABLE", body.Error.Code)
	}
}

func TestQRCodeRoutes(t *testing.T) {
	handler, manager := newTestHandler(t, fakeHealth{})

	created := do(t, handler, http.MethodPost, "/api/v1/tenants/tenant-a/sessions", testToken, `{"name":"Vendas"}`)
	session := decode[sessionResponse](t, created)
	item := "/api/v1/tenants/tenant-a/sessions/" + session.ID

	// Sem QR gerado ainda: 409, nÃ£o 500 â€” o bot deve aguardar.
	manager.qrErr = wa.ErrNoQRCode
	rec := do(t, handler, http.MethodGet, item+"/qr", testToken, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("sem QR: status = %d, esperado 409", rec.Code)
	}
	if body := decode[errorBody](t, rec); body.Error.Code != "QR_NOT_AVAILABLE" {
		t.Errorf("code = %q, esperado QR_NOT_AVAILABLE", body.Error.Code)
	}

	expires := time.Now().UTC().Add(20 * time.Second)
	manager.qrErr = nil
	manager.qr = wa.QRCode{Payload: "2@payload", Image: "data:image/png;base64,AAA", ExpiresAt: expires}

	ok := do(t, handler, http.MethodGet, item+"/qr", testToken, "")
	if ok.Code != http.StatusOK {
		t.Fatalf("com QR: status = %d, esperado 200", ok.Code)
	}
	body := decode[qrResponse](t, ok)
	if body.QR != "2@payload" || body.Image == "" {
		t.Errorf("payload do QR incompleto: %+v", body)
	}
	if !body.ExpiresAt.Equal(expires) {
		t.Errorf("expiresAt = %v, esperado %v", body.ExpiresAt, expires)
	}
}

func TestTenantIsolationOverHTTP(t *testing.T) {
	handler, _ := newTestHandler(t, fakeHealth{})

	created := do(t, handler, http.MethodPost, "/api/v1/tenants/tenant-a/sessions", testToken, `{"name":"Vendas"}`)
	session := decode[sessionResponse](t, created)

	rec := do(t, handler, http.MethodGet, "/api/v1/tenants/tenant-b/sessions/"+session.ID, testToken, "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("sessÃ£o de outro tenant: status = %d, esperado 404", rec.Code)
	}
}

func TestCreateSessionValidation(t *testing.T) {
	handler, _ := newTestHandler(t, fakeHealth{})
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

func TestSendTextRoute(t *testing.T) {
	handler, manager := newTestHandler(t, fakeHealth{})
	created := do(t, handler, http.MethodPost, "/api/v1/tenants/tenant-a/sessions", testToken, `{"name":"Vendas"}`)
	session := decode[sessionResponse](t, created)
	item := "/api/v1/tenants/tenant-a/sessions/" + session.ID

	rec := do(t, handler, http.MethodPost, item+"/messages", testToken, `{"to":"11999999999","text":"bom dia"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, esperado 200 (%s)", rec.Code, rec.Body.String())
	}
	if !decode[map[string]bool](t, rec)["sent"] {
		t.Error("resposta deveria conter sent=true")
	}
	if manager.lastText.To != "11999999999" || manager.lastText.Text != "bom dia" {
		t.Errorf("payload repassado = %+v", manager.lastText)
	}
}

func TestSendTextErrors(t *testing.T) {
	cases := map[string]struct {
		err        error
		wantStatus int
		wantCode   string
	}{
		"texto vazio":         {wa.ErrEmptyText, http.StatusBadRequest, "VALIDATION_ERROR"},
		"nÃºmero invÃ¡lido":   {wa.ErrInvalidNumber, http.StatusBadRequest, "VALIDATION_ERROR"},
		"sessÃ£o caÃ­da":      {wa.ErrSessionNotReady, http.StatusConflict, "SESSION_NOT_CONNECTED"},
		"sessÃ£o nÃ£o existe": {sessions.ErrNotFound, http.StatusNotFound, "SESSION_NOT_FOUND"},
		"falha no WhatsApp":   {errors.New("boom"), http.StatusBadGateway, "WHATSAPP_UNAVAILABLE"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			handler, manager := newTestHandler(t, fakeHealth{})
			manager.sendTextErr = tc.err

			rec := do(t, handler, http.MethodPost, "/api/v1/tenants/tenant-a/sessions/qualquer/messages", testToken,
				`{"to":"11999999999","text":"oi"}`)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, esperado %d", rec.Code, tc.wantStatus)
			}
			if body := decode[errorBody](t, rec); body.Error.Code != tc.wantCode {
				t.Errorf("code = %q, esperado %q", body.Error.Code, tc.wantCode)
			}
		})
	}
}

func TestSendMediaRoute(t *testing.T) {
	handler, manager := newTestHandler(t, fakeHealth{})
	item := "/api/v1/tenants/tenant-a/sessions/sessao-1"

	rec := do(t, handler, http.MethodPost, item+"/messages/media", testToken,
		`{"to":"11999999999","mediaUrl":"https://exemplo/foto.jpg","mime":"image/jpeg","caption":"segue"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, esperado 200 (%s)", rec.Code, rec.Body.String())
	}
	if manager.lastMedia.MediaURL != "https://exemplo/foto.jpg" || manager.lastMedia.Caption != "segue" {
		t.Errorf("payload repassado = %+v", manager.lastMedia)
	}

	manager.sendMediaErr = wa.ErrMediaTooLarge
	big := do(t, handler, http.MethodPost, item+"/messages/media", testToken,
		`{"to":"11999999999","mediaUrl":"https://exemplo/grande.jpg"}`)
	if big.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("mÃ­dia grande: status = %d, esperado 413", big.Code)
	}
	if body := decode[errorBody](t, big); body.Error.Code != "MEDIA_TOO_LARGE" {
		t.Errorf("code = %q, esperado MEDIA_TOO_LARGE", body.Error.Code)
	}
}

func TestPresenceRouteNeverBlocks(t *testing.T) {
	handler, manager := newTestHandler(t, fakeHealth{})
	item := "/api/v1/tenants/tenant-a/sessions/sessao-1"

	manager.presenceErr = wa.ErrSessionNotReady
	rec := do(t, handler, http.MethodPost, item+"/presence", testToken, `{"to":"11999999999","type":"typing"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("presenÃ§a com falha: status = %d, esperado 200", rec.Code)
	}
	if decode[map[string]bool](t, rec)["sent"] {
		t.Error("sent deveria ser false quando a presenÃ§a falha")
	}
	if manager.presence != wa.PresenceTyping {
		t.Errorf("presenÃ§a repassada = %q", manager.presence)
	}

	manager.presenceErr = sessions.ErrNotFound
	missing := do(t, handler, http.MethodPost, item+"/presence", testToken, `{"to":"11999999999","type":"typing"}`)
	if missing.Code != http.StatusNotFound {
		t.Errorf("sessÃ£o inexistente: status = %d, esperado 404", missing.Code)
	}
}
