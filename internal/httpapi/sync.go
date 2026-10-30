package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/rodrigovmoreira/Squamata-whatsapp/internal/labels"
	"github.com/rodrigovmoreira/Squamata-whatsapp/internal/sessions"
	"github.com/rodrigovmoreira/Squamata-whatsapp/internal/wa"
)

// F5 — sincronização de contatos (substitui o scraping via pupPage).

type contactResponse struct {
	JID      string `json:"jid"`
	Phone    string `json:"phone"`
	Name     string `json:"name,omitempty"`
	PushName string `json:"pushName,omitempty"`
}

type contactsSyncResponse struct {
	Contacts  []contactResponse `json:"contacts"`
	Total     int               `json:"total"`
	Truncated bool              `json:"truncated"`
}

func (a *API) handleSyncContacts(w http.ResponseWriter, r *http.Request) {
	tenantID, sessionID := r.PathValue("tenantId"), r.PathValue("id")

	contacts, truncated, err := a.manager.SyncContacts(r.Context(), tenantID, sessionID)
	if err != nil {
		a.writeSyncError(w, err, tenantID, sessionID, "sincronizar contatos")
		return
	}

	payload := contactsSyncResponse{
		Contacts:  make([]contactResponse, 0, len(contacts)),
		Total:     len(contacts),
		Truncated: truncated,
	}
	for _, c := range contacts {
		payload.Contacts = append(payload.Contacts, contactResponse{
			JID: c.JID, Phone: c.Phone, Name: c.Name, PushName: c.PushName,
		})
	}
	a.writeJSON(w, http.StatusOK, payload)
}

// F6 — etiquetas (app state).

type labelResponse struct {
	LabelID string `json:"labelId"`
	Name    string `json:"name"`
	Color   int32  `json:"color"`
}

type labelRequest struct {
	Name  string `json:"name"`
	Color *int32 `json:"color"`
}

func toLabelResponse(l labels.Label) labelResponse {
	return labelResponse{LabelID: l.LabelID, Name: l.Name, Color: l.Color}
}

func (a *API) handleListLabels(w http.ResponseWriter, r *http.Request) {
	tenantID, sessionID := r.PathValue("tenantId"), r.PathValue("id")

	list, err := a.manager.ListLabels(r.Context(), tenantID, sessionID)
	if err != nil {
		a.writeSyncError(w, err, tenantID, sessionID, "listar etiquetas")
		return
	}

	payload := make([]labelResponse, 0, len(list))
	for _, l := range list {
		payload = append(payload, toLabelResponse(l))
	}
	a.writeJSON(w, http.StatusOK, payload)
}

func (a *API) handleCreateLabel(w http.ResponseWriter, r *http.Request) {
	tenantID, sessionID := r.PathValue("tenantId"), r.PathValue("id")

	var req labelRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.writeError(w, http.StatusBadRequest, "INVALID_BODY", err.Error())
		return
	}

	var color int32
	if req.Color != nil {
		color = *req.Color
	}

	created, err := a.manager.UpsertLabel(r.Context(), tenantID, sessionID, "", req.Name, color)
	if err != nil {
		a.writeSyncError(w, err, tenantID, sessionID, "criar etiqueta")
		return
	}
	a.writeJSON(w, http.StatusCreated, toLabelResponse(created))
}

func (a *API) handleUpdateLabel(w http.ResponseWriter, r *http.Request) {
	tenantID, sessionID := r.PathValue("tenantId"), r.PathValue("id")
	labelID := r.PathValue("labelId")

	var req labelRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.writeError(w, http.StatusBadRequest, "INVALID_BODY", err.Error())
		return
	}

	var color int32
	if req.Color != nil {
		color = *req.Color
	}

	updated, err := a.manager.UpsertLabel(r.Context(), tenantID, sessionID, labelID, req.Name, color)
	if err != nil {
		a.writeSyncError(w, err, tenantID, sessionID, "editar etiqueta")
		return
	}
	a.writeJSON(w, http.StatusOK, toLabelResponse(updated))
}

func (a *API) handleDeleteLabel(w http.ResponseWriter, r *http.Request) {
	tenantID, sessionID := r.PathValue("tenantId"), r.PathValue("id")
	labelID := r.PathValue("labelId")

	if err := a.manager.DeleteLabel(r.Context(), tenantID, sessionID, labelID); err != nil {
		a.writeSyncError(w, err, tenantID, sessionID, "remover etiqueta")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) handleLabelChats(w http.ResponseWriter, r *http.Request) {
	tenantID, sessionID := r.PathValue("tenantId"), r.PathValue("id")
	labelID := r.PathValue("labelId")

	chats, err := a.manager.LabelChats(r.Context(), tenantID, sessionID, labelID)
	if err != nil {
		a.writeSyncError(w, err, tenantID, sessionID, "listar chats da etiqueta")
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"chats": chats, "total": len(chats)})
}

type setChatLabelsRequest struct {
	LabelIDs []string `json:"labelIds"`
}

func (a *API) handleSetChatLabels(w http.ResponseWriter, r *http.Request) {
	tenantID, sessionID := r.PathValue("tenantId"), r.PathValue("id")
	chatID := r.PathValue("chatId")

	var req setChatLabelsRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.writeError(w, http.StatusBadRequest, "INVALID_BODY", err.Error())
		return
	}

	if err := a.manager.SetChatLabels(r.Context(), tenantID, sessionID, chatID, req.LabelIDs); err != nil {
		a.writeSyncError(w, err, tenantID, sessionID, "aplicar etiquetas no chat")
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]bool{"updated": true})
}

// writeSyncError reaproveita o vocabulário do contrato para F5/F6.
func (a *API) writeSyncError(w http.ResponseWriter, err error, tenantID, sessionID, action string) {
	switch {
	case errors.Is(err, sessions.ErrNotFound):
		a.writeError(w, http.StatusNotFound, "SESSION_NOT_FOUND", "sessão não encontrada")
	case errors.Is(err, wa.ErrSessionNotReady):
		a.writeError(w, http.StatusConflict, "SESSION_NOT_CONNECTED",
			"a sessão não está conectada; conecte e aguarde o status ready")
	case errors.Is(err, wa.ErrEmptyLabel):
		a.writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
	case errors.Is(err, wa.ErrInvalidNumber):
		a.writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
	case strings.Contains(err.Error(), "not-a-business"):
		a.writeError(w, http.StatusConflict, "NOT_A_BUSINESS_ACCOUNT",
			"a conta conectada não é WhatsApp Business; etiquetas exigem conta Business")
	default:
		a.logger.Error("falha na operação de WhatsApp",
			"acao", action, "tenantId", tenantID, "sessionId", sessionID, "erro", err)
		a.writeError(w, http.StatusBadGateway, "WHATSAPP_UNAVAILABLE",
			"não foi possível completar a operação no WhatsApp agora")
	}
}
