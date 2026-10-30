package httpapi

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/rodrigovmoreira/Squamata-whatsapp/internal/tenants"
)

// Authenticator resolve quem está chamando. Dois níveis:
//
//   - token raiz (SQUAMATA_INTERNAL_TOKEN): operação do ecossistema, acessa todos
//     os tenants. Existe para bootstrap e para a migração do Calango-bot.
//   - token de tenant: acesso restrito ao próprio tenant.
//
// Sem isso, qualquer consumidor com o token único leria sessões dos outros.
type Authenticator struct {
	rootToken string
	tenants   *tenants.Repo
	logger    *slog.Logger
}

type contextKey string

const (
	contextTenantID contextKey = "tenant-id"
	contextIsRoot   contextKey = "is-root"
)

func NewAuthenticator(rootToken string, repo *tenants.Repo, logger *slog.Logger) *Authenticator {
	if logger == nil {
		logger = slog.Default()
	}
	return &Authenticator{rootToken: rootToken, tenants: repo, logger: logger}
}

// middleware valida a credencial e guarda no contexto quem chamou.
func (au *Authenticator) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			writeUnauthorized(w)
			return
		}

		if au.rootToken != "" && subtle.ConstantTimeCompare([]byte(token), []byte(au.rootToken)) == 1 {
			ctx := context.WithValue(r.Context(), contextIsRoot, true)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		tenant, err := au.tenants.GetByToken(r.Context(), token)
		if err != nil {
			if errors.Is(err, tenants.ErrNotFound) {
				au.logger.Warn("token interno inválido", "rota", r.URL.Path, "origem", r.RemoteAddr)
				writeUnauthorized(w)
				return
			}
			au.logger.Error("falha ao resolver token", "erro", err)
			writeUnauthorized(w)
			return
		}

		ctx := context.WithValue(r.Context(), contextTenantID, tenant.ID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// tenantScope impede que o token de um tenant acesse o caminho de outro.
// É aplicado por rota porque o PathValue só existe depois do casamento do padrão.
func (au *Authenticator) tenantScope(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if isRoot(r) {
			next(w, r)
			return
		}

		allowed := tenantFromContext(r)
		requested := r.PathValue("tenantId")
		// Comparação em tempo constante também aqui: o tenantId vem do caminho.
		if allowed == "" || subtle.ConstantTimeCompare([]byte(allowed), []byte(requested)) != 1 {
			au.logger.Warn("acesso a tenant não autorizado",
				"tokenTenant", allowed, "tenantRequisitado", requested, "rota", r.URL.Path)
			writeJSONError(w, http.StatusForbidden, "TENANT_FORBIDDEN",
				"o token apresentado não tem acesso a este tenant")
			return
		}
		next(w, r)
	}
}

// rootOnly protege as rotas de administração de tenants.
func (au *Authenticator) rootOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isRoot(r) {
			writeJSONError(w, http.StatusForbidden, "ROOT_TOKEN_REQUIRED",
				"esta operação exige o token raiz do serviço")
			return
		}
		next(w, r)
	}
}

func bearerToken(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, prefix) {
		return "", false
	}
	return strings.TrimPrefix(header, prefix), true
}

func isRoot(r *http.Request) bool {
	root, _ := r.Context().Value(contextIsRoot).(bool)
	return root
}

func tenantFromContext(r *http.Request) string {
	id, _ := r.Context().Value(contextTenantID).(string)
	return id
}

func writeUnauthorized(w http.ResponseWriter) {
	writeJSONError(w, http.StatusUnauthorized, "UNAUTHORIZED", "token interno ausente ou inválido")
}
