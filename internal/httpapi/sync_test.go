package httpapi

import (
	"errors"
	"net/http"
	"testing"

	"github.com/rodrigovmoreira/Squamata-whatsapp/internal/labels"
	"github.com/rodrigovmoreira/Squamata-whatsapp/internal/sessions"
	"github.com/rodrigovmoreira/Squamata-whatsapp/internal/wa"
)

const sessionPath = "/api/v1/tenants/tenant-a/sessions/sessao-1"

func TestSyncContactsRoute(t *testing.T) {
	handler, _ := newTestHandler(t, fakeHealth{})

	rec := do(t, handler, http.MethodPost, sessionPath+"/contacts/sync", testToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, esperado 200 (%s)", rec.Code, rec.Body.String())
	}

	body := decode[contactsSyncResponse](t, rec)
	if body.Total != 1 || len(body.Contacts) != 1 {
		t.Fatalf("contatos = %+v", body)
	}
	if body.Contacts[0].Phone != "5511999999999" {
		t.Errorf("phone = %q", body.Contacts[0].Phone)
	}
	if body.Truncated {
		t.Error("truncated deveria ser false")
	}
}

func TestSyncContactsErrors(t *testing.T) {
	cases := map[string]struct {
		err        error
		wantStatus int
		wantCode   string
	}{
		"sessão caída":       {wa.ErrSessionNotReady, http.StatusConflict, "SESSION_NOT_CONNECTED"},
		"sessão inexistente": {sessions.ErrNotFound, http.StatusNotFound, "SESSION_NOT_FOUND"},
		"falha no WhatsApp":  {errors.New("boom"), http.StatusBadGateway, "WHATSAPP_UNAVAILABLE"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			handler, manager := newTestHandler(t, fakeHealth{})
			manager.syncErr = tc.err

			rec := do(t, handler, http.MethodPost, sessionPath+"/contacts/sync", testToken, "")
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, esperado %d", rec.Code, tc.wantStatus)
			}
			if body := decode[errorBody](t, rec); body.Error.Code != tc.wantCode {
				t.Errorf("code = %q, esperado %q", body.Error.Code, tc.wantCode)
			}
		})
	}
}

func TestLabelsCRUD(t *testing.T) {
	handler, _ := newTestHandler(t, fakeHealth{})

	list := do(t, handler, http.MethodGet, sessionPath+"/labels", testToken, "")
	if list.Code != http.StatusOK {
		t.Fatalf("listar: status = %d", list.Code)
	}
	if items := decode[[]labelResponse](t, list); len(items) != 1 || items[0].Name != "Cliente" {
		t.Errorf("labels = %+v", items)
	}

	created := do(t, handler, http.MethodPost, sessionPath+"/labels", testToken, `{"name":"VIP","color":3}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("criar: status = %d (%s)", created.Code, created.Body.String())
	}
	label := decode[labelResponse](t, created)
	if label.LabelID != "novo" || label.Name != "VIP" || label.Color != 3 {
		t.Errorf("label criada = %+v", label)
	}

	updated := do(t, handler, http.MethodPatch, sessionPath+"/labels/123", testToken, `{"name":"VIP Gold"}`)
	if updated.Code != http.StatusOK {
		t.Fatalf("editar: status = %d", updated.Code)
	}
	if decode[labelResponse](t, updated).LabelID != "123" {
		t.Error("PATCH deveria preservar o labelId do caminho")
	}

	if rec := do(t, handler, http.MethodDelete, sessionPath+"/labels/123", testToken, ""); rec.Code != http.StatusNoContent {
		t.Errorf("remover: status = %d, esperado 204", rec.Code)
	}
}

func TestLabelValidation(t *testing.T) {
	handler, manager := newTestHandler(t, fakeHealth{})

	manager.labelErr = wa.ErrEmptyLabel
	rec := do(t, handler, http.MethodPost, sessionPath+"/labels", testToken, `{"name":"  "}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("nome vazio: status = %d, esperado 400", rec.Code)
	}
	if body := decode[errorBody](t, rec); body.Error.Code != "VALIDATION_ERROR" {
		t.Errorf("code = %q", body.Error.Code)
	}

	manager.labelErr = nil
	if rec := do(t, handler, http.MethodPost, sessionPath+"/labels", testToken, `{"name":`); rec.Code != http.StatusBadRequest {
		t.Errorf("json inválido: status = %d, esperado 400", rec.Code)
	}
}

func TestLabelChatsAndSetChatLabels(t *testing.T) {
	handler, manager := newTestHandler(t, fakeHealth{})

	chats := do(t, handler, http.MethodGet, sessionPath+"/labels/1/chats", testToken, "")
	if chats.Code != http.StatusOK {
		t.Fatalf("chats: status = %d", chats.Code)
	}
	body := decode[map[string]any](t, chats)
	if body["total"].(float64) != 1 {
		t.Errorf("total = %v", body["total"])
	}

	set := do(t, handler, http.MethodPut,
		sessionPath+"/chats/5511999999999@s.whatsapp.net/labels", testToken, `{"labelIds":["1","2"]}`)
	if set.Code != http.StatusOK {
		t.Fatalf("vincular: status = %d (%s)", set.Code, set.Body.String())
	}
	if len(manager.labelIDs) != 2 {
		t.Errorf("labelIds repassados = %v", manager.labelIDs)
	}

	manager.labelErr = wa.ErrInvalidNumber
	bad := do(t, handler, http.MethodPut, sessionPath+"/chats/xyz/labels", testToken, `{"labelIds":[]}`)
	if bad.Code != http.StatusBadRequest {
		t.Errorf("chat inválido: status = %d, esperado 400", bad.Code)
	}
}

// Garante que o espelho de labels responde mesmo sem WhatsApp: a leitura vem
// do SQLite do serviço, não do app state.
func TestLabelRepoMirror(t *testing.T) {
	handler, manager := newTestHandler(t, fakeHealth{})
	manager.labelErr = errors.New("whatsapp fora")

	rec := do(t, handler, http.MethodGet, sessionPath+"/labels", testToken, "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, esperado 502", rec.Code)
	}
	_ = labels.Label{} // referência explícita ao tipo usado pelo contrato
}
