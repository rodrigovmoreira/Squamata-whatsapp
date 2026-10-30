package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/rodrigovmoreira/Squamata-whatsapp/internal/tenants"
)

// Administração de tenants: só o token raiz enxerga e altera.

type tenantResponse struct {
	ID         string `json:"id"`
	WebhookURL string `json:"webhookUrl,omitempty"`
	HasSecret  bool   `json:"hasWebhookSecret"`
}

type createTenantRequest struct {
	ID            string `json:"id"`
	WebhookURL    string `json:"webhookUrl"`
	WebhookSecret string `json:"webhookSecret"`
}

type updateTenantRequest struct {
	WebhookURL    string `json:"webhookUrl"`
	WebhookSecret string `json:"webhookSecret"`
}

func toTenantResponse(t tenants.Tenant) tenantResponse {
	return tenantResponse{ID: t.ID, WebhookURL: t.WebhookURL, HasSecret: t.WebhookSecret != ""}
}

func (a *API) handleCreateTenant(w http.ResponseWriter, r *http.Request) {
	var req createTenantRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.writeError(w, http.StatusBadRequest, "INVALID_BODY", err.Error())
		return
	}

	req.ID = strings.TrimSpace(req.ID)
	if req.ID == "" {
		a.writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "id do tenant é obrigatório")
		return
	}

	// O token só é devolvido uma vez: o banco guarda apenas o hash.
	token := newTenantToken()
	created, err := a.tenants.Create(r.Context(), req.ID, token, req.WebhookURL, req.WebhookSecret)
	if err != nil {
		if errors.Is(err, tenants.ErrDuplicateID) {
			a.writeError(w, http.StatusConflict, "TENANT_EXISTS", "tenant já existe")
			return
		}
		a.logger.Error("falha ao criar tenant", "tenantId", req.ID, "erro", err)
		a.writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "não foi possível criar o tenant")
		return
	}

	payload := toTenantResponse(created)
	a.writeJSON(w, http.StatusCreated, map[string]any{
		"id":               payload.ID,
		"webhookUrl":       payload.WebhookURL,
		"hasWebhookSecret": payload.HasSecret,
		"token":            token,
	})
}

func (a *API) handleListTenants(w http.ResponseWriter, r *http.Request) {
	list, err := a.tenants.List(r.Context())
	if err != nil {
		a.logger.Error("falha ao listar tenants", "erro", err)
		a.writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "não foi possível listar os tenants")
		return
	}

	payload := make([]tenantResponse, 0, len(list))
	for _, t := range list {
		payload = append(payload, toTenantResponse(t))
	}
	a.writeJSON(w, http.StatusOK, payload)
}

func (a *API) handleUpdateTenant(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("tenantId")

	var req updateTenantRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.writeError(w, http.StatusBadRequest, "INVALID_BODY", err.Error())
		return
	}

	updated, err := a.tenants.UpdateWebhook(r.Context(), tenantID, req.WebhookURL, req.WebhookSecret)
	if err != nil {
		if errors.Is(err, tenants.ErrNotFound) {
			a.writeError(w, http.StatusNotFound, "TENANT_NOT_FOUND", "tenant não encontrado")
			return
		}
		a.logger.Error("falha ao atualizar tenant", "tenantId", tenantID, "erro", err)
		a.writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "não foi possível atualizar o tenant")
		return
	}
	// Sem isso, o dispatcher continuaria publicando no webhook antigo por 60 s.
	a.tenants.InvalidateWebhook(tenantID)
	a.writeJSON(w, http.StatusOK, toTenantResponse(updated))
}

func newTenantToken() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		// rand.Read só falha em ambiente quebrado; o boot já falharia antes.
		panic("gerar token do tenant: " + err.Error())
	}
	return hex.EncodeToString(buf)
}
