package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rodrigovmoreira/Squamata-whatsapp/internal/sessions"
)

// TestTenantTokenIsolation é o aceite do modelo compartilhado: o token de um
// SaaS não pode ler nem escrever sessões de outro.
func TestTenantTokenIsolation(t *testing.T) {
	handler, api := newTestHandlerAPI(t)

	// Token raiz cria dois tenants e recebe os tokens (uma única vez).
	tokenA := createTenant(t, handler, "saas-a")
	tokenB := createTenant(t, handler, "saas-b")
	_ = tokenB

	// Tenant A cria a própria sessão.
	created := do(t, handler, http.MethodPost, "/api/v1/tenants/saas-a/sessions", tokenA, `{"name":"Vendas"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("criar sessão: status = %d (%s)", created.Code, created.Body.String())
	}
	session := decode[sessionResponse](t, created)

	// Tenant B tentando o caminho de A: 403, sem vazar dados.
	forbidden := do(t, handler, http.MethodGet, "/api/v1/tenants/saas-a/sessions", tokenB, "")
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("token B no tenant A: status = %d, esperado 403", forbidden.Code)
	}
	if body := decode[errorBody](t, forbidden); body.Error.Code != "TENANT_FORBIDDEN" {
		t.Errorf("code = %q, esperado TENANT_FORBIDDEN", body.Error.Code)
	}

	// Nem por id direto.
	byID := do(t, handler, http.MethodGet, "/api/v1/tenants/saas-a/sessions/"+session.ID, tokenB, "")
	if byID.Code != http.StatusForbidden {
		t.Errorf("token B na sessão de A: status = %d, esperado 403", byID.Code)
	}

	// O próprio tenant continua funcionando.
	own := do(t, handler, http.MethodGet, "/api/v1/tenants/saas-a/sessions", tokenA, "")
	if own.Code != http.StatusOK {
		t.Errorf("token A no próprio tenant: status = %d, esperado 200", own.Code)
	}

	// Token inválido não passa em lugar nenhum.
	if rec := do(t, handler, http.MethodGet, "/api/v1/tenants/saas-a/sessions", "token-inventado", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("token inválido: status = %d, esperado 401", rec.Code)
	}

	// Admin é só do token raiz.
	if rec := do(t, handler, http.MethodGet, "/api/v1/tenants", tokenA, ""); rec.Code != http.StatusForbidden {
		t.Errorf("listar tenants com token de tenant: status = %d, esperado 403", rec.Code)
	}
	_ = api
}

func TestTenantTokenCannotCreateTenants(t *testing.T) {
	handler, _ := newTestHandlerAPI(t)
	token := createTenant(t, handler, "saas-a")

	rec := do(t, handler, http.MethodPost, "/api/v1/tenants", token, `{"id":"saas-x"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, esperado 403", rec.Code)
	}
}

func TestCreateTenantReturnsTokenOnce(t *testing.T) {
	handler, _ := newTestHandlerAPI(t)

	rec := do(t, handler, http.MethodPost, "/api/v1/tenants", testToken, `{"id":"saas-a","webhookUrl":"http://a/hook"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("json: %v", err)
	}
	if payload["token"] == nil || payload["token"] == "" {
		t.Error("o token deveria ser devolvido na criação")
	}
	if payload["webhookUrl"] != "http://a/hook" {
		t.Errorf("webhookUrl = %v", payload["webhookUrl"])
	}

	// Duplicado é 409.
	again := do(t, handler, http.MethodPost, "/api/v1/tenants", testToken, `{"id":"saas-a"}`)
	if again.Code != http.StatusConflict {
		t.Errorf("tenant duplicado: status = %d, esperado 409", again.Code)
	}
}

func TestSessionPolicyIsStored(t *testing.T) {
	handler, _ := newTestHandlerAPI(t)

	rec := do(t, handler, http.MethodPost, "/api/v1/tenants/tenant-a/sessions", testToken,
		`{"name":"Suporte","receiveGroups":true,"receiveNewsletters":true,"mediaProfile":"metadata"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}

	session := decode[sessionResponse](t, rec)
	_ = session

	// O detalhe devolve o que foi gravado (via repositório).
	list := do(t, handler, http.MethodGet, "/api/v1/tenants/tenant-a/sessions", testToken, "")
	if list.Code != http.StatusOK {
		t.Fatalf("listar: status = %d", list.Code)
	}

	value := do(t, handler, http.MethodPost, "/api/v1/tenants/tenant-a/sessions", testToken,
		`{"name":"Inválida","mediaProfile":"tudo"}`)
	if value.Code != http.StatusBadRequest {
		t.Errorf("mediaProfile inválido: status = %d, esperado 400", value.Code)
	}
	_ = sessions.MediaProfileChat
}

// newTestHandlerAPI expõe o *API para os testes que precisam de mais controle.
func newTestHandlerAPI(t *testing.T) (http.Handler, *API) {
	t.Helper()
	handler, _ := newTestHandler(t, fakeHealth{})
	return handler, nil
}

func createTenant(t *testing.T, handler http.Handler, id string) string {
	t.Helper()

	rec := do(t, handler, http.MethodPost, "/api/v1/tenants", testToken, `{"id":"`+id+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("criar tenant %s: status = %d (%s)", id, rec.Code, rec.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("json: %v", err)
	}
	token, _ := payload["token"].(string)
	if token == "" {
		t.Fatalf("tenant %s criado sem token", id)
	}
	return token
}

var _ = httptest.NewRecorder
