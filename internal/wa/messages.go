package wa

import (
	"context"
	"encoding/base64"
	"strings"
	"time"

	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	waEvents "go.mau.fi/whatsmeow/types/events"

	svcEvents "github.com/rodrigovmoreira/Squamata-whatsapp/internal/events"
	"github.com/rodrigovmoreira/Squamata-whatsapp/internal/sessions"
)

const (
	// maxMediaBytes é o teto de mídia decodificada (D1).
	maxMediaBytes = 8 << 20
	mediaTimeout  = 60 * time.Second
	// mediaSlots limita downloads simultâneos (D1: semáforo de 4).
	mediaSlots = 4
	// maxIDDigits é o "Iron Gate" herdado: IDs técnicos de comunidade passam
	// de 15 dígitos e não são telefone.
	maxIDDigits = 15
)

type kind string

const (
	kindText     kind = "text"
	kindImage    kind = "image"
	kindAudio    kind = "audio"
	kindVideo    kind = "video"
	kindDocument kind = "document"
	kindOther    kind = "other"
)

type content struct {
	Kind     kind
	Text     string
	Mime     string
	Filename string
	Size     uint64
	Seconds  *uint32
}

// extractContent é pura de propósito: é a parte que mais erra e a que os testes
// cobrem sem precisar de WhatsApp.
func extractContent(msg *waE2E.Message) content {
	if msg == nil {
		return content{Kind: kindOther}
	}

	if text := msg.GetConversation(); text != "" {
		return content{Kind: kindText, Text: text}
	}
	if text := msg.GetExtendedTextMessage().GetText(); text != "" {
		return content{Kind: kindText, Text: text}
	}

	if image := msg.GetImageMessage(); image != nil {
		return content{
			Kind:     kindImage,
			Text:     image.GetCaption(),
			Mime:     image.GetMimetype(),
			Filename: "imagem" + extensionFor(image.GetMimetype()),
			Size:     image.GetFileLength(),
		}
	}
	if audio := msg.GetAudioMessage(); audio != nil {
		c := content{
			Kind:     kindAudio,
			Mime:     audio.GetMimetype(),
			Filename: "audio" + extensionFor(audio.GetMimetype()),
			Size:     audio.GetFileLength(),
		}
		if seconds := audio.GetSeconds(); seconds > 0 {
			c.Seconds = &seconds
		}
		return c
	}
	if video := msg.GetVideoMessage(); video != nil {
		c := content{
			Kind:     kindVideo,
			Text:     video.GetCaption(),
			Mime:     video.GetMimetype(),
			Filename: "video" + extensionFor(video.GetMimetype()),
			Size:     video.GetFileLength(),
		}
		if seconds := video.GetSeconds(); seconds > 0 {
			c.Seconds = &seconds
		}
		return c
	}
	if document := msg.GetDocumentMessage(); document != nil {
		name := document.GetFileName()
		if name == "" {
			name = "documento"
		}
		return content{
			Kind:     kindDocument,
			Text:     document.GetCaption(),
			Mime:     document.GetMimetype(),
			Filename: name,
			Size:     document.GetFileLength(),
		}
	}

	return content{Kind: kindOther}
}

func extensionFor(mime string) string {
	switch {
	case strings.Contains(mime, "jpeg"):
		return ".jpg"
	case strings.Contains(mime, "png"):
		return ".png"
	case strings.Contains(mime, "webp"):
		return ".webp"
	case strings.Contains(mime, "ogg"), strings.Contains(mime, "opus"):
		return ".ogg"
	case strings.Contains(mime, "mp4"):
		return ".mp4"
	case strings.Contains(mime, "mpeg"):
		return ".mp3"
	default:
		return ""
	}
}

// shouldIgnore reproduz o "Iron Gate" do adaptador antigo: grupos, status,
// canais e IDs técnicos longos não entram no CRM.
// shouldIgnore aplica a política da sessão: grupos, canais e broadcast são
// conteúdo que cada consumidor decide receber ou não.
func shouldIgnore(info types.MessageInfo, policy sessions.Policy) bool {
	if info.IsGroup && !policy.ReceiveGroups {
		return true
	}
	switch info.Chat.Server {
	case types.GroupServer:
		if !policy.ReceiveGroups {
			return true
		}
	case types.BroadcastServer:
		if !policy.ReceiveBroadcast {
			return true
		}
	case types.NewsletterServer:
		if !policy.ReceiveNewsletters {
			return true
		}
	}
	return len(digitsOnly(info.Sender.User)) > maxIDDigits
}

func digitsOnly(value string) string {
	var b strings.Builder
	for _, r := range value {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// onMessage trata entrada e saída com o mesmo caminho: `fromMe` decide se o
// evento é `message.sent` (enviado pelo celular) ou `message`.
func (m *Manager) onMessage(ctx context.Context, st *managed, evt *waEvents.Message) {
	info := evt.Info
	if shouldIgnore(info, st.policy) {
		return
	}

	body := extractContent(evt.Message)
	if body.Kind == kindOther {
		// Sticker, localização, contato, reação: nada útil para o CRM.
		return
	}
	if body.Kind == kindText && strings.TrimSpace(body.Text) == "" {
		return
	}

	phone, raw := m.resolveSender(ctx, st, info)

	// Eco da própria API: se fomos nós que enviamos, o bot já gravou a
	// mensagem no histórico dele — emitir `message.sent` duplicaria.
	if info.IsFromMe && m.consumeSent(st.sessionID, string(info.ID)) {
		return
	}
	media := m.buildMedia(ctx, st, evt.Message, body)

	eventName := svcEvents.Message
	if info.IsFromMe {
		eventName = svcEvents.MessageSent
	}

	stamp := info.Timestamp
	if stamp.IsZero() {
		stamp = time.Now().UTC()
	}

	m.sink.Emit(svcEvents.Event{
		Event:     eventName,
		TenantID:  st.tenantID,
		SessionID: st.sessionID,
		MessageID: string(info.ID),
		From:      phone,
		RawFrom:   raw,
		PushName:  info.PushName,
		Type:      string(body.Kind),
		Text:      body.Text,
		Media:     media,
		FromMe:    info.IsFromMe,
		Timestamp: stamp.UTC(),
	})
}

// resolveSender devolve o telefone (só dígitos) e o JID original. Em sessão
// com addressing mode LID o identificador visível é um @lid, que não é
// telefone: tentamos resolver para PN e mantemos o original em rawFrom.
func (m *Manager) resolveSender(ctx context.Context, st *managed, info types.MessageInfo) (phone, raw string) {
	source := info.Sender
	if info.IsFromMe {
		// Em mensagem enviada por nós, o interlocutor é o chat.
		source = info.Chat
	}

	raw = source.String()
	phone = source.User

	if source.Server == types.HiddenUserServer || source.Server == types.HostedLIDServer {
		pn, err := st.client.Store.LIDs.GetPNForLID(ctx, source)
		if err != nil {
			m.logger.Warn("falha ao resolver @lid para telefone",
				"sessionId", st.sessionID, "jid", raw, "erro", err)
			return phone, raw
		}
		if !pn.IsEmpty() {
			phone = pn.User
		}
	}
	return phone, raw
}

// buildMedia baixa bytes apenas de imagem e áudio (D1). Vídeo e documento vão
// como metadado; acima de 8 MB a mensagem sai sem bytes e com tooLarge.
func (m *Manager) buildMedia(ctx context.Context, st *managed, msg *waE2E.Message, body content) *svcEvents.Media {
	if body.Kind == kindText || body.Kind == kindOther {
		return nil
	}

	media := &svcEvents.Media{
		Mime:     body.Mime,
		Filename: body.Filename,
		Size:     body.Size,
		Seconds:  body.Seconds,
	}

	if body.Kind != kindImage && body.Kind != kindAudio {
		// Metadado, sem baixar bytes: o custo de base64 não se justifica.
		return media
	}

	// Política de mídia por sessão: consumidores que não precisam de bytes
	// recebem só os metadados.
	if st.policy.MediaProfile == string(sessions.MediaProfileMetadata) {
		return media
	}

	maxBytes := st.policy.MaxMediaBytes
	if maxBytes <= 0 {
		maxBytes = sessions.DefaultMaxMediaBytes
	}

	if body.Size > uint64(maxBytes) {
		media.TooLarge = true
		return media
	}

	select {
	case m.mediaSem <- struct{}{}:
		defer func() { <-m.mediaSem }()
	case <-time.After(mediaTimeout):
		media.TooLarge = true
		m.logger.Warn("tempo de espera para baixar mídia excedido", "sessionId", st.sessionID)
		return media
	}

	downloadCtx, cancel := context.WithTimeout(ctx, mediaTimeout)
	defer cancel()

	raw, err := st.client.DownloadAny(downloadCtx, msg)
	if err != nil {
		m.logger.Warn("falha ao baixar mídia", "sessionId", st.sessionID,
			"tipo", body.Kind, "erro", err)
		return media
	}
	if len(raw) > maxMediaBytes {
		media.TooLarge = true
		return media
	}

	media.Size = uint64(len(raw))
	media.Base64 = base64.StdEncoding.EncodeToString(raw)
	return media
}
