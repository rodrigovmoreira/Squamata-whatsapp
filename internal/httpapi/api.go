package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/rodrigovmoreira/Squamata-whatsapp/internal/sessions"
)

const maxBodyBytes = 64 << 10

// HealthChecker é satisfeito pelo store. Interface para que o teste de health
// possa simular um banco indisponível sem derrubar o SQLite.
type HealthChecker interface {
	Health(ctx context.Context) error
}

type API struct {
	repo   *sessions.Repo
	store  HealthChecker
	token  string
	logger *slog.Logger
}

func New(repo *sessions.Repo, store HealthChecker, token string, logger *slog.Logger) *API {
	if logger == nil {
		logger = slog.Default()
	}
	return &API{repo: repo, store: store, token: token, logger: logger}
}

// Handler monta o roteamento. Só /health é público; todo o resto exige o
// token de serviço para serviço.
func (a *API) Handler() http.Handler {
	protected := http.NewServeMux()
	protected.HandleFunc("POST /api/v1/tenants/{tenantId}/sessions", a.handleCreateSession)
	protected.HandleFunc("GET /api/v1/tenants/{tenantId}/sessions", a.handleListSessions)
	protected.HandleFunc("GET /api/v1/tenants/{tenantId}/sessions/{id}", a.handleGetSession)
	protected.HandleFunc("DELETE /api/v1/tenants/{tenantId}/sessions/{id}", a.handleDeleteSession)

	root := http.NewServeMux()
	root.HandleFunc("GET /api/v1/health", a.handleHealth)
	root.Handle("/", a.requireToken(protected))
	return root
}

func (a *API) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, prefix) {
			a.writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "token interno ausente ou malformado")
			return
		}
		given := strings.TrimPrefix(header, prefix)
		if subtle.ConstantTimeCompare([]byte(given), []byte(a.token)) != 1 {
			a.logger.Warn("token interno inválido", "rota", r.URL.Path, "origem", r.RemoteAddr)
			a.writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "token interno inválido")
			return
		}
		next.ServeHTTP(w, r)
	})
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
}

func (a *API) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("tenantId")

	var req createSessionRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.writeError(w, http.StatusBadRequest, "INVALID_BODY", err.Error())
		return
	}

	created, err := a.repo.Create(r.Context(), sessions.Session{
		TenantID:    tenantID,
		Name:        req.Name,
		OwnerUserID: req.OwnerUserID,
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

	if err := a.repo.Delete(r.Context(), tenantID, id); err != nil {
		if errors.Is(err, sessions.ErrNotFound) {
			a.writeError(w, http.StatusNotFound, "SESSION_NOT_FOUND", "sessão não encontrada")
			return
		}
		a.logger.Error("falha ao remover sessão", "tenantId", tenantID, "sessionId", id, "erro", err)
		a.writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "não foi possível remover a sessão")
		return
	}

	w.WriteHeader(http.StatusNoContent)
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
		errors.Is(err, sessions.ErrNameTooLong)
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (a *API) writeError(w http.ResponseWriter, status int, code, message string) {
	a.writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: message}})
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
