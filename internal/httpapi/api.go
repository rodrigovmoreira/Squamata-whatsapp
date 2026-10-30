package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/rodrigovmoreira/Squamata-whatsapp/internal/labels"
	"github.com/rodrigovmoreira/Squamata-whatsapp/internal/sessions"
	"github.com/rodrigovmoreira/Squamata-whatsapp/internal/tenants"
	"github.com/rodrigovmoreira/Squamata-whatsapp/internal/wa"
)

const maxBodyBytes = 64 << 10

// HealthChecker é satisfeito pelo store. Interface para que o teste de health
// possa simular um banco indisponível sem derrubar o SQLite.
type HealthChecker interface {
	Health(ctx context.Context) error
}

// SessionManager é a máquina de estados do pareamento mais o envio. Interface
// para que o teste de HTTP rode sem whatsmeow e sem rede.
type SessionManager interface {
	Connect(ctx context.Context, tenantID, sessionID string) (sessions.Session, error)
	Disconnect(ctx context.Context, tenantID, sessionID string) (sessions.Session, error)
	Delete(ctx context.Context, tenantID, sessionID string) error
	QRCode(tenantID, sessionID string) (wa.QRCode, error)
	SendText(ctx context.Context, tenantID, sessionID, to, text string) (string, error)
	SendMedia(ctx context.Context, tenantID, sessionID, to, mediaURL, mime, caption string) (string, error)
	SendPresence(ctx context.Context, tenantID, sessionID, to string, kind wa.PresenceKind) error

	SyncContacts(ctx context.Context, tenantID, sessionID string) ([]wa.Contact, bool, error)
	ListLabels(ctx context.Context, tenantID, sessionID string) ([]labels.Label, error)
	UpsertLabel(ctx context.Context, tenantID, sessionID, labelID, name string, color int32) (labels.Label, error)
	DeleteLabel(ctx context.Context, tenantID, sessionID, labelID string) error
	SetChatLabels(ctx context.Context, tenantID, sessionID, chatJID string, labelIDs []string) error
	LabelChats(ctx context.Context, tenantID, sessionID, labelID string) ([]string, error)
}

type API struct {
	repo    *sessions.Repo
	store   HealthChecker
	manager SessionManager
	tenants *tenants.Repo
	auth    *Authenticator
	logger  *slog.Logger
}

func New(repo *sessions.Repo, store HealthChecker, manager SessionManager, tenantsRepo *tenants.Repo, token string, logger *slog.Logger) *API {
	if logger == nil {
		logger = slog.Default()
	}
	return &API{
		repo:    repo,
		store:   store,
		manager: manager,
		tenants: tenantsRepo,
		auth:    NewAuthenticator(token, tenantsRepo, logger),
		logger:  logger,
	}
}

// Handler monta o roteamento. Só /health é público. As rotas de tenant exigem
// credencial válida E escopo no tenant do caminho; as de administração, token raiz.
func (a *API) Handler() http.Handler {
	protected := http.NewServeMux()
	protected.HandleFunc("POST /api/v1/tenants", a.auth.rootOnly(a.handleCreateTenant))
	protected.HandleFunc("GET /api/v1/tenants", a.auth.rootOnly(a.handleListTenants))
	protected.HandleFunc("PATCH /api/v1/tenants/{tenantId}", a.auth.rootOnly(a.handleUpdateTenant))

	protected.HandleFunc("POST /api/v1/tenants/{tenantId}/sessions", a.auth.tenantScope(a.handleCreateSession))
	protected.HandleFunc("GET /api/v1/tenants/{tenantId}/sessions", a.auth.tenantScope(a.handleListSessions))
	protected.HandleFunc("GET /api/v1/tenants/{tenantId}/sessions/{id}", a.auth.tenantScope(a.handleGetSession))
	protected.HandleFunc("DELETE /api/v1/tenants/{tenantId}/sessions/{id}", a.auth.tenantScope(a.handleDeleteSession))
	protected.HandleFunc("POST /api/v1/tenants/{tenantId}/sessions/{id}/connect", a.auth.tenantScope(a.handleConnect))
	protected.HandleFunc("POST /api/v1/tenants/{tenantId}/sessions/{id}/disconnect", a.auth.tenantScope(a.handleDisconnect))
	protected.HandleFunc("GET /api/v1/tenants/{tenantId}/sessions/{id}/qr", a.auth.tenantScope(a.handleQRCode))
	protected.HandleFunc("POST /api/v1/tenants/{tenantId}/sessions/{id}/messages", a.auth.tenantScope(a.handleSendText))
	protected.HandleFunc("POST /api/v1/tenants/{tenantId}/sessions/{id}/messages/media", a.auth.tenantScope(a.handleSendMedia))
	protected.HandleFunc("POST /api/v1/tenants/{tenantId}/sessions/{id}/presence", a.auth.tenantScope(a.handlePresence))
	protected.HandleFunc("POST /api/v1/tenants/{tenantId}/sessions/{id}/contacts/sync", a.auth.tenantScope(a.handleSyncContacts))
	protected.HandleFunc("GET /api/v1/tenants/{tenantId}/sessions/{id}/labels", a.auth.tenantScope(a.handleListLabels))
	protected.HandleFunc("POST /api/v1/tenants/{tenantId}/sessions/{id}/labels", a.auth.tenantScope(a.handleCreateLabel))
	protected.HandleFunc("PATCH /api/v1/tenants/{tenantId}/sessions/{id}/labels/{labelId}", a.auth.tenantScope(a.handleUpdateLabel))
	protected.HandleFunc("DELETE /api/v1/tenants/{tenantId}/sessions/{id}/labels/{labelId}", a.auth.tenantScope(a.handleDeleteLabel))
	protected.HandleFunc("GET /api/v1/tenants/{tenantId}/sessions/{id}/labels/{labelId}/chats", a.auth.tenantScope(a.handleLabelChats))
	protected.HandleFunc("PUT /api/v1/tenants/{tenantId}/sessions/{id}/chats/{chatId}/labels", a.auth.tenantScope(a.handleSetChatLabels))

	root := http.NewServeMux()
	root.HandleFunc("GET /api/v1/health", a.handleHealth)
	root.Handle("/", a.auth.middleware(protected))
	return root
}

type healthResponse struct {
	Status    string        `json:"status"`
	Service   string        `json:"service"`
	Store     storeHealth   `json:"store"`
	Sessions  sessionHealth `json:"sessions"`
	Timestamp time.Time     `json:"timestamp"`
}

type storeHealth struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

type sessionHealth struct {
	Total    int            `json:"total"`
	ByStatus map[string]int `json:"byStatus"`
}

// handleHealth responde pelo processo E pelo banco: um "está vivo" sem store
// acessível não serve para o healthcheck do container.
func (a *API) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	body := healthResponse{Service: "squamata-whatsapp", Timestamp: time.Now().UTC()}
	body.Sessions.ByStatus = map[string]int{}

	storeErr := a.store.Health(ctx)
	counts, countErr := a.repo.CountByStatus(ctx)

	if storeErr != nil || countErr != nil {
		body.Status = "DOWN"
		body.Store.OK = false
		switch {
		case storeErr != nil:
			body.Store.Error = storeErr.Error()
		default:
			body.Store.Error = countErr.Error()
		}
		a.writeJSON(w, http.StatusServiceUnavailable, body)
		return
	}

	body.Status = "UP"
	body.Store.OK = true
	body.Sessions.ByStatus = counts
	for _, total := range counts {
		body.Sessions.Total += total
	}
	a.writeJSON(w, http.StatusOK, body)
}

type createSessionRequest struct {
	Name        string `json:"name"`
	OwnerUserID string `json:"ownerUserId"`

	// Política de conteúdo (F-multi-SaaS): cada consumidor decide o que recebe.
	ReceiveGroups      bool   `json:"receiveGroups"`
	ReceiveNewsletters bool   `json:"receiveNewsletters"`
	ReceiveBroadcast   bool   `json:"receiveBroadcast"`
	MediaProfile       string `json:"mediaProfile"`
	MaxMediaBytes      int64  `json:"maxMediaBytes"`
}

func (a *API) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("tenantId")

	var req createSessionRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.writeError(w, http.StatusBadRequest, "INVALID_BODY", err.Error())
		return
	}

	created, err := a.repo.Create(r.Context(), sessions.Session{
		TenantID:           tenantID,
		Name:               req.Name,
		OwnerUserID:        req.OwnerUserID,
		ReceiveGroups:      req.ReceiveGroups,
		ReceiveNewsletters: req.ReceiveNewsletters,
		ReceiveBroadcast:   req.ReceiveBroadcast,
		MediaProfile:       req.MediaProfile,
		MaxMediaBytes:      req.MaxMediaBytes,
	})
	if err != nil {
		if isValidation(err) {
			a.writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
			return
		}
		a.logger.Error("falha ao criar sessão", "tenantId", tenantID, "erro", err)
		a.writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "não foi possível criar a sessão")
		return
	}

	a.writeJSON(w, http.StatusCreated, toResponse(created))
}

func (a *API) handleListSessions(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("tenantId")

	list, err := a.repo.List(r.Context(), tenantID)
	if err != nil {
		a.logger.Error("falha ao listar sessões", "tenantId", tenantID, "erro", err)
		a.writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "não foi possível listar as sessões")
		return
	}

	payload := make([]sessionResponse, 0, len(list))
	for _, s := range list {
		payload = append(payload, toResponse(s))
	}
	a.writeJSON(w, http.StatusOK, payload)
}

func (a *API) handleGetSession(w http.ResponseWriter, r *http.Request) {
	a.withSession(w, r, func(s sessions.Session) {
		a.writeJSON(w, http.StatusOK, toResponse(s))
	})
}

func (a *API) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	tenantID, id := r.PathValue("tenantId"), r.PathValue("id")

	// O gerenciador faz logout, remove o device do sqlstore e só então apaga
	// o registro — é o único caminho que descarta o device (decisão D6).
	if err := a.manager.Delete(r.Context(), tenantID, id); err != nil {
		a.writeManagerError(w, err, tenantID, id, "remover")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (a *API) handleConnect(w http.ResponseWriter, r *http.Request) {
	tenantID, id := r.PathValue("tenantId"), r.PathValue("id")

	updated, err := a.manager.Connect(r.Context(), tenantID, id)
	if err != nil {
		a.writeManagerError(w, err, tenantID, id, "conectar")
		return
	}
	a.writeJSON(w, http.StatusOK, toResponse(updated))
}

func (a *API) handleDisconnect(w http.ResponseWriter, r *http.Request) {
	tenantID, id := r.PathValue("tenantId"), r.PathValue("id")

	updated, err := a.manager.Disconnect(r.Context(), tenantID, id)
	if err != nil {
		a.writeManagerError(w, err, tenantID, id, "desconectar")
		return
	}
	a.writeJSON(w, http.StatusOK, toResponse(updated))
}

type qrResponse struct {
	QR        string    `json:"qr"`
	Image     string    `json:"image"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func (a *API) handleQRCode(w http.ResponseWriter, r *http.Request) {
	tenantID, id := r.PathValue("tenantId"), r.PathValue("id")

	code, err := a.manager.QRCode(tenantID, id)
	if err != nil {
		a.writeManagerError(w, err, tenantID, id, "obter QR")
		return
	}

	a.writeJSON(w, http.StatusOK, qrResponse{
		QR:        code.Payload,
		Image:     code.Image,
		ExpiresAt: code.ExpiresAt,
	})
}

// writeManagerError traduz os erros do gerenciador no envelope do contrato.
func (a *API) writeManagerError(w http.ResponseWriter, err error, tenantID, sessionID, action string) {
	switch {
	case errors.Is(err, sessions.ErrNotFound):
		a.writeError(w, http.StatusNotFound, "SESSION_NOT_FOUND", "sessão não encontrada")
	case errors.Is(err, wa.ErrNoQRCode):
		a.writeError(w, http.StatusConflict, "QR_NOT_AVAILABLE",
			"não há QR válido no momento; conecte a sessão e aguarde a geração")
	default:
		a.logger.Error("falha na operação de sessão",
			"acao", action, "tenantId", tenantID, "sessionId", sessionID, "erro", err)
		a.writeError(w, http.StatusBadGateway, "WHATSAPP_UNAVAILABLE",
			"não foi possível falar com o WhatsApp agora")
	}
}

type sendTextRequest struct {
	To   string `json:"to"`
	Text string `json:"text"`
}

func (a *API) handleSendText(w http.ResponseWriter, r *http.Request) {
	tenantID, sessionID := r.PathValue("tenantId"), r.PathValue("id")

	var req sendTextRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.writeError(w, http.StatusBadRequest, "INVALID_BODY", err.Error())
		return
	}

	messageID, err := a.manager.SendText(r.Context(), tenantID, sessionID, req.To, req.Text)
	if err != nil {
		a.writeSendError(w, err, tenantID, sessionID, "enviar texto")
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"sent": true, "messageId": messageID})
}

type sendMediaRequest struct {
	To       string `json:"to"`
	MediaURL string `json:"mediaUrl"`
	Mime     string `json:"mime"`
	Caption  string `json:"caption"`
}

func (a *API) handleSendMedia(w http.ResponseWriter, r *http.Request) {
	tenantID, sessionID := r.PathValue("tenantId"), r.PathValue("id")

	var req sendMediaRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.writeError(w, http.StatusBadRequest, "INVALID_BODY", err.Error())
		return
	}

	messageID, err := a.manager.SendMedia(r.Context(), tenantID, sessionID, req.To, req.MediaURL, req.Mime, req.Caption)
	if err != nil {
		a.writeSendError(w, err, tenantID, sessionID, "enviar mídia")
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"sent": true, "messageId": messageID})
}

type presenceRequest struct {
	To   string `json:"to"`
	Type string `json:"type"`
}

func (a *API) handlePresence(w http.ResponseWriter, r *http.Request) {
	tenantID, sessionID := r.PathValue("tenantId"), r.PathValue("id")

	var req presenceRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.writeError(w, http.StatusBadRequest, "INVALID_BODY", err.Error())
		return
	}

	err := a.manager.SendPresence(r.Context(), tenantID, sessionID, req.To, wa.PresenceKind(req.Type))
	switch {
	case err == nil:
		a.writeJSON(w, http.StatusOK, map[string]bool{"sent": true})
	case errors.Is(err, sessions.ErrNotFound):
		a.writeError(w, http.StatusNotFound, "SESSION_NOT_FOUND", "sessão não encontrada")
	default:
		// Presença é cosmética: o envio da mensagem não depende dela.
		a.logger.Warn("presença não enviada",
			"tenantId", tenantID, "sessionId", sessionID, "erro", err)
		a.writeJSON(w, http.StatusOK, map[string]bool{"sent": false})
	}
}

// writeSendError traduz as falhas de envio nos códigos do contrato.
func (a *API) writeSendError(w http.ResponseWriter, err error, tenantID, sessionID, action string) {
	switch {
	case errors.Is(err, sessions.ErrNotFound):
		a.writeError(w, http.StatusNotFound, "SESSION_NOT_FOUND", "sessão não encontrada")
	case errors.Is(err, wa.ErrSessionNotReady):
		a.writeError(w, http.StatusConflict, "SESSION_NOT_CONNECTED",
			"a sessão não está conectada; conecte e aguarde o status ready")
	case errors.Is(err, wa.ErrEmptyText), errors.Is(err, wa.ErrInvalidNumber),
		errors.Is(err, wa.ErrUnsupportedMime):
		a.writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
	case errors.Is(err, wa.ErrMediaTooLarge):
		a.writeError(w, http.StatusRequestEntityTooLarge, "MEDIA_TOO_LARGE",
			"a mídia excede o limite de 8 MB")
	default:
		a.logger.Error("falha ao enviar mensagem",
			"acao", action, "tenantId", tenantID, "sessionId", sessionID, "erro", err)
		a.writeError(w, http.StatusBadGateway, "WHATSAPP_UNAVAILABLE",
			"não foi possível enviar pelo WhatsApp agora")
	}
}

func (a *API) withSession(w http.ResponseWriter, r *http.Request, fn func(sessions.Session)) {
	tenantID, id := r.PathValue("tenantId"), r.PathValue("id")

	s, err := a.repo.Get(r.Context(), tenantID, id)
	switch {
	case errors.Is(err, sessions.ErrNotFound):
		a.writeError(w, http.StatusNotFound, "SESSION_NOT_FOUND", "sessão não encontrada")
	case err != nil:
		a.logger.Error("falha ao buscar sessão", "tenantId", tenantID, "sessionId", id, "erro", err)
		a.writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "não foi possível buscar a sessão")
	default:
		fn(s)
	}
}

type sessionResponse struct {
	ID          string     `json:"id"`
	TenantID    string     `json:"tenantId"`
	OwnerUserID string     `json:"ownerUserId,omitempty"`
	Name        string     `json:"name"`
	Phone       string     `json:"phone,omitempty"`
	Status      string     `json:"status"`
	LastError   string     `json:"lastError,omitempty"`
	ConnectedAt *time.Time `json:"connectedAt,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
}

func toResponse(s sessions.Session) sessionResponse {
	return sessionResponse{
		ID:          s.ID,
		TenantID:    s.TenantID,
		OwnerUserID: s.OwnerUserID,
		Name:        s.Name,
		Phone:       s.Phone,
		Status:      string(s.Status),
		LastError:   s.LastError,
		ConnectedAt: s.ConnectedAt,
		CreatedAt:   s.CreatedAt,
		UpdatedAt:   s.UpdatedAt,
	}
}

func isValidation(err error) bool {
	return errors.Is(err, sessions.ErrEmptyTenant) ||
		errors.Is(err, sessions.ErrEmptyName) ||
		errors.Is(err, sessions.ErrNameTooLong) ||
		errors.Is(err, sessions.ErrInvalidMediaProfile)
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (a *API) writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSONError(w, status, code, message)
}

// writeJSONError é a versão de pacote, usada pelos middlewares de autenticação.
func writeJSONError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{Error: errorDetail{Code: code, Message: message}})
}

func (a *API) writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		a.logger.Error("falha ao escrever resposta", "erro", err)
	}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dest any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(dest); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("corpo da requisição vazio")
		}
		return errors.New("JSON inválido")
	}
	return nil
}
