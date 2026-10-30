package wa

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"github.com/rodrigovmoreira/Squamata-whatsapp/internal/sessions"
)

const (
	// sendTimeout cobre o envio em si (D4: síncrono, 30 s).
	sendTimeout = 30 * time.Second
	// mediaSendTimeout soma download + upload: 30 s não bastam.
	mediaSendTimeout = 90 * time.Second
	// maxOutboundMediaBytes é o mesmo teto da entrada (D1).
	maxOutboundMediaBytes = 8 << 20
)

var (
	ErrSessionNotReady = errors.New("sessão não está conectada")
	ErrInvalidNumber   = errors.New("número de destino inválido")
	ErrEmptyText       = errors.New("texto da mensagem é obrigatório")
	ErrMediaTooLarge   = errors.New("mídia acima do limite permitido")
	ErrUnsupportedMime = errors.New("tipo de mídia não suportado no envio")
)

// PresenceKind é o que o bot pede em POST .../presence.
type PresenceKind string

const (
	PresenceTyping PresenceKind = "typing"
	PresencePaused PresenceKind = "paused"
)

func (k PresenceKind) Valid() bool {
	return k == PresenceTyping || k == PresencePaused
}

// SendText envia texto para um contato. Reflete o `sendWWebJSMessage` antigo:
// aceita número nacional (adiciona 55) e internacional.
// Devolve o ID da mensagem no WhatsApp (usado para dedup do eco — ver onMessage).
func (m *Manager) SendText(ctx context.Context, tenantID, sessionID, to, text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", ErrEmptyText
	}

	st, jid, err := m.prepareSend(ctx, tenantID, sessionID, to)
	if err != nil {
		return "", err
	}

	sendCtx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()

	resp, err := st.client.SendMessage(sendCtx, jid, &waE2E.Message{
		Conversation: proto.String(text),
	})
	if err != nil {
		return "", fmt.Errorf("enviar texto para %s: %w", jid.String(), err)
	}

	messageID := string(resp.ID)
	m.rememberSent(st.sessionID, messageID)
	return messageID, nil
}

// SendMedia baixa a imagem da URL e envia com legenda. O bot nunca manda bytes:
// ele passa a URL (campanha/Firebase ou IA), como fazia com o axios no Node.
func (m *Manager) SendMedia(ctx context.Context, tenantID, sessionID, to, mediaURL, mime, caption string) (string, error) {
	if strings.TrimSpace(mediaURL) == "" {
		return "", fmt.Errorf("%w: mediaUrl é obrigatório", ErrUnsupportedMime)
	}

	st, jid, err := m.prepareSend(ctx, tenantID, sessionID, to)
	if err != nil {
		return "", err
	}

	sendCtx, cancel := context.WithTimeout(ctx, mediaSendTimeout)
	defer cancel()

	data, contentType, err := downloadMedia(sendCtx, mediaURL, mime)
	if err != nil {
		return "", err
	}
	if contentType != "image/jpeg" && contentType != "image/png" && contentType != "image/webp" {
		return "", fmt.Errorf("%w: %s", ErrUnsupportedMime, contentType)
	}

	uploaded, err := st.client.Upload(sendCtx, data, whatsmeow.MediaImage)
	if err != nil {
		return "", fmt.Errorf("subir mídia para o WhatsApp: %w", err)
	}

	image := &waE2E.ImageMessage{
		Mimetype:      proto.String(contentType),
		URL:           proto.String(uploaded.URL),
		DirectPath:    proto.String(uploaded.DirectPath),
		MediaKey:      uploaded.MediaKey,
		FileEncSHA256: uploaded.FileEncSHA256,
		FileSHA256:    uploaded.FileSHA256,
		FileLength:    proto.Uint64(uploaded.FileLength),
	}
	if caption != "" {
		image.Caption = proto.String(caption)
	}

	resp, err := st.client.SendMessage(sendCtx, jid, &waE2E.Message{ImageMessage: image})
	if err != nil {
		return "", fmt.Errorf("enviar imagem para %s: %w", jid.String(), err)
	}

	messageID := string(resp.ID)
	m.rememberSent(st.sessionID, messageID)
	return messageID, nil
}

// SendPresence é cosmético: "digitando…" ou pausado. Nunca impede o envio da
// mensagem — o handler da API ignora falhas aqui (só registra no log).
func (m *Manager) SendPresence(ctx context.Context, tenantID, sessionID, to string, kind PresenceKind) error {
	if !kind.Valid() {
		return fmt.Errorf("tipo de presença inválido: %q", kind)
	}

	st, jid, err := m.prepareSend(ctx, tenantID, sessionID, to)
	if err != nil {
		return err
	}

	state := types.ChatPresenceComposing
	if kind == PresencePaused {
		state = types.ChatPresencePaused
	}

	sendCtx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()

	if err := st.client.SendChatPresence(sendCtx, jid, state, types.ChatPresenceMediaText); err != nil {
		return fmt.Errorf("enviar presença para %s: %w", jid.String(), err)
	}
	return nil
}

func (m *Manager) prepareSend(ctx context.Context, tenantID, sessionID, to string) (*managed, types.JID, error) {
	// Validar o destino primeiro: número inválido é erro de entrada (400),
	// independente do estado da sessão.
	jid, err := parseTarget(to)
	if err != nil {
		return nil, types.JID{}, err
	}

	st := m.lookup(sessionID)
	if st == nil {
		// Não há client em memória. Pode ser sessão inexistente ou sessão que
		// existe mas nunca foi conectada (serviço reiniciado): só o repositório
		// distingue — sem isso o bot receberia 404 e desistiria em vez de 409.
		if _, repoErr := m.repo.Get(ctx, tenantID, sessionID); repoErr != nil {
			return nil, types.JID{}, repoErr
		}
		return nil, types.JID{}, ErrSessionNotReady
	}
	// Um sessionId de outro tenant não resolve — mesma regra do repositório.
	if st.tenantID != tenantID {
		return nil, types.JID{}, sessions.ErrNotFound
	}
	if !st.client.IsConnected() || !st.client.IsLoggedIn() {
		return nil, types.JID{}, ErrSessionNotReady
	}

	return st, jid, nil
}

// parseTarget espelha a regra antiga do wwebjsService: número nacional (até 12
// dígitos) ganha o DDI 55; número já internacional fica como está.
func parseTarget(to string) (types.JID, error) {
	trimmed := strings.TrimSpace(to)
	if trimmed == "" {
		return types.JID{}, ErrInvalidNumber
	}

	if strings.Contains(trimmed, "@") {
		jid, err := types.ParseJID(trimmed)
		if err != nil || jid.User == "" {
			return types.JID{}, ErrInvalidNumber
		}
		return jid, nil
	}

	digits := digitsOnly(trimmed)
	if len(digits) < 10 || len(digits) > 15 {
		return types.JID{}, ErrInvalidNumber
	}
	if len(digits) <= 12 {
		digits = "55" + digits
	}
	return types.NewJID(digits, types.DefaultUserServer), nil
}

// downloadMedia busca a imagem na URL informada respeitando o teto de 8 MB.
func downloadMedia(ctx context.Context, mediaURL, mimeHint string) ([]byte, string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, mediaURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("URL de mídia inválida: %w", err)
	}

	response, err := (&http.Client{Timeout: mediaSendTimeout}).Do(request)
	if err != nil {
		return nil, "", fmt.Errorf("baixar mídia: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("baixar mídia: servidor respondeu %d", response.StatusCode)
	}

	// Lemos no máximo um byte além do teto: o suficiente para detectar excesso
	// sem carregar um arquivo gigante na memória.
	data, err := io.ReadAll(io.LimitReader(response.Body, maxOutboundMediaBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("ler mídia: %w", err)
	}
	if len(data) > maxOutboundMediaBytes {
		return nil, "", ErrMediaTooLarge
	}

	contentType := strings.TrimSpace(strings.Split(mimeHint, ";")[0])
	if contentType == "" {
		contentType = strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0])
	}
	if contentType == "" || contentType == "application/octet-stream" {
		contentType = guessImageMime(mediaURL)
	}

	return data, contentType, nil
}

func guessImageMime(mediaURL string) string {
	lower := strings.ToLower(mediaURL)
	switch {
	case strings.HasSuffix(lower, ".png"):
		return "image/png"
	case strings.HasSuffix(lower, ".webp"):
		return "image/webp"
	default:
		return "image/jpeg"
	}
}
