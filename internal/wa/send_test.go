package wa

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"

	"github.com/rodrigovmoreira/Squamata-whatsapp/internal/sessions"

	_ "modernc.org/sqlite"
)

func TestParseTarget(t *testing.T) {
	cases := map[string]struct {
		input     string
		wantUser  string
		wantError error
	}{
		"número nacional ganha DDI":       {"11999999999", "5511999999999", nil},
		"número internacional fica igual": {"5511999999999", "5511999999999", nil},
		"já é JID completo":               {"5511999999999@s.whatsapp.net", "5511999999999", nil},
		"com máscara":                     {"+55 (11) 99999-9999", "5511999999999", nil},
		"curto demais":                    {"123", "", ErrInvalidNumber},
		"vazio":                           {"   ", "", ErrInvalidNumber},
		"letras":                          {"abc", "", ErrInvalidNumber},
		"longo demais":                    {"1234567890123456", "", ErrInvalidNumber},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			jid, err := parseTarget(tc.input)

			if tc.wantError != nil {
				if !errors.Is(err, tc.wantError) {
					t.Fatalf("erro = %v, esperado %v", err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("erro inesperado: %v", err)
			}
			if jid.User != tc.wantUser {
				t.Errorf("user = %q, esperado %q", jid.User, tc.wantUser)
			}
			if jid.Server != types.DefaultUserServer {
				t.Errorf("server = %q, esperado %q", jid.Server, types.DefaultUserServer)
			}
		})
	}
}

func TestPresenceKindValid(t *testing.T) {
	if !PresenceTyping.Valid() || !PresencePaused.Valid() {
		t.Error("typing e paused deveriam ser válidos")
	}
	if PresenceKind("digitando").Valid() {
		t.Error("valor desconhecido não deveria ser válido")
	}
}

func TestDownloadMedia(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok.jpg":
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write([]byte("bytes-da-imagem"))
		case "/sem-tipo":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write([]byte("bytes"))
		case "/quebrado":
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	t.Run("usa o mime do servidor", func(t *testing.T) {
		data, mime, err := downloadMedia(context.Background(), server.URL+"/ok.jpg", "")
		if err != nil {
			t.Fatalf("erro: %v", err)
		}
		if string(data) != "bytes-da-imagem" {
			t.Errorf("data = %q", data)
		}
		if mime != "image/jpeg" {
			t.Errorf("mime = %q", mime)
		}
	})

	t.Run("mime explícito vence o do servidor", func(t *testing.T) {
		_, mime, err := downloadMedia(context.Background(), server.URL+"/ok.jpg", "image/png; charset=binary")
		if err != nil {
			t.Fatalf("erro: %v", err)
		}
		if mime != "image/png" {
			t.Errorf("mime = %q, esperado image/png", mime)
		}
	})

	t.Run("octet-stream cai para a extensão", func(t *testing.T) {
		_, mime, err := downloadMedia(context.Background(), server.URL+"/sem-tipo", "")
		if err != nil {
			t.Fatalf("erro: %v", err)
		}
		if mime != "image/jpeg" {
			t.Errorf("mime = %q, esperado image/jpeg (fallback)", mime)
		}
	})

	t.Run("erro HTTP não vira imagem", func(t *testing.T) {
		if _, _, err := downloadMedia(context.Background(), server.URL+"/quebrado", ""); err == nil {
			t.Fatal("esperava erro para status 500")
		}
	})

	t.Run("acima de 8 MB é recusado", func(t *testing.T) {
		big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write([]byte(strings.Repeat("x", maxOutboundMediaBytes+10)))
		}))
		defer big.Close()

		if _, _, err := downloadMedia(context.Background(), big.URL, ""); !errors.Is(err, ErrMediaTooLarge) {
			t.Fatalf("erro = %v, esperado ErrMediaTooLarge", err)
		}
	})
}

// TestPrepareSendWithoutClient é regressão de um bug pego no teste manual:
// sessão que existe no banco mas não tem client em memória respondia 404
// (SESSION_NOT_FOUND) em vez de 409 (SESSION_NOT_CONNECTED), e o bot desistiria
// em vez de avisar que basta conectar.
func TestPrepareSendWithoutClient(t *testing.T) {
	dir, err := os.MkdirTemp("", "squamata-send-")
	if err != nil {
		t.Fatalf("criar diretório: %v", err)
	}
	t.Cleanup(func() {
		for attempt := 0; attempt < 5; attempt++ {
			if err := os.RemoveAll(dir); err == nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	})

	dsn := "file:" + filepath.ToSlash(filepath.Join(dir, "send.db")) + "?_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("abrir sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	repo := sessions.NewRepo(db)
	ctx := context.Background()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	created, err := repo.Create(ctx, sessions.Session{TenantID: "tenant-a", Name: "Vendas"})
	if err != nil {
		t.Fatalf("criar sessão: %v", err)
	}

	m := &Manager{
		repo:    repo,
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		clients: make(map[string]*managed),
	}

	if _, _, err := m.prepareSend(ctx, "tenant-a", created.ID, "11999999999"); !errors.Is(err, ErrSessionNotReady) {
		t.Errorf("sessão sem client: erro = %v, esperado ErrSessionNotReady", err)
	}
	if _, _, err := m.prepareSend(ctx, "tenant-b", created.ID, "11999999999"); !errors.Is(err, sessions.ErrNotFound) {
		t.Errorf("outro tenant: erro = %v, esperado ErrNotFound", err)
	}
	if _, _, err := m.prepareSend(ctx, "tenant-a", "nao-existe", "11999999999"); !errors.Is(err, sessions.ErrNotFound) {
		t.Errorf("sessão inexistente: erro = %v, esperado ErrNotFound", err)
	}
	if _, _, err := m.prepareSend(ctx, "tenant-a", created.ID, "abc"); !errors.Is(err, ErrInvalidNumber) {
		t.Errorf("número inválido: erro = %v, esperado ErrInvalidNumber", err)
	}
}
