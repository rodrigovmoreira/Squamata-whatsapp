package sessions

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Status Ã© o estado da sessÃ£o. Os valores sÃ£o exatamente os que o painel do
// Calango-bot jÃ¡ recebe hoje (getSessionStatus), para nÃ£o quebrar o frontend.
type Status string

const (
	StatusDisconnected  Status = "disconnected"
	StatusInitializing  Status = "initializing"
	StatusQRCode        Status = "qrcode"
	StatusAuthenticated Status = "authenticated"
	StatusReady         Status = "ready"
	StatusDisconnecting Status = "disconnecting"
	StatusError         Status = "error"
)

func (s Status) Valid() bool {
	switch s {
	case StatusDisconnected, StatusInitializing, StatusQRCode,
		StatusAuthenticated, StatusReady, StatusDisconnecting, StatusError:
		return true
	}
	return false
}

// Active indica que existe client vivo para a sessÃ£o. Usado para que connect
// seja idempotente em vez de criar um segundo client para o mesmo nÃºmero.
func (s Status) Active() bool {
	switch s {
	case StatusInitializing, StatusQRCode, StatusAuthenticated, StatusReady, StatusDisconnecting:
		return true
	}
	return false
}

var (
	ErrNotFound      = errors.New("sessÃ£o nÃ£o encontrada")
	ErrEmptyTenant   = errors.New("tenantId Ã© obrigatÃ³rio")
	ErrEmptyName     = errors.New("nome da sessÃ£o Ã© obrigatÃ³rio")
	ErrNameTooLong   = errors.New("nome da sessÃ£o deve ter no mÃ¡ximo 80 caracteres")
	ErrInvalidStatus = errors.New("status de sessÃ£o invÃ¡lido")
	// ErrInvalidMediaProfile: mediaProfile precisa ser "chat" ou "metadata".
	ErrInvalidMediaProfile = errors.New("mediaProfile invÃ¡lido (use chat ou metadata)")
)

const maxNameLength = 80

func (s Status) valid() bool { return s.Valid() }

// MediaProfile define quanto de mÃ­dia o serviÃ§o baixa antes de publicar o evento.
//   - "chat"     (padrÃ£o): bytes de image/audio atÃ© MaxMediaBytes
//   - "metadata": nenhum byte, apenas metadados
//
// Ã‰ configuraÃ§Ã£o por sessÃ£o porque cada SaaS tem um consumidor com requisitos
// prÃ³prios (D1 nasceu do Calango-bot, nÃ£o Ã© regra do ecossistema).
type MediaProfile string

const (
	MediaProfileChat     MediaProfile = "chat"
	MediaProfileMetadata MediaProfile = "metadata"

	// DefaultMaxMediaBytes Ã© o teto herdado da D1 (8 MB).
	DefaultMaxMediaBytes int64 = 8 << 20
)

func (p MediaProfile) valid() bool {
	return p == MediaProfileChat || p == MediaProfileMetadata
}

// Policy reÃºne o que varia por consumidor. O default reproduz o comportamento
// do Calango-bot: sem grupos, canais nem broadcast, mÃ­dia de chat atÃ© 8 MB.
type Policy struct {
	ReceiveGroups      bool
	ReceiveNewsletters bool
	ReceiveBroadcast   bool
	MediaProfile       string
	MaxMediaBytes      int64
}

func DefaultPolicy() Policy {
	return Policy{
		MediaProfile:  string(MediaProfileChat),
		MaxMediaBytes: DefaultMaxMediaBytes,
	}
}

// Normalize preenche defaults e valida.
func (p *Policy) Normalize() error {
	if p.MediaProfile == "" {
		p.MediaProfile = string(MediaProfileChat)
	}
	if !MediaProfile(p.MediaProfile).valid() {
		return ErrInvalidMediaProfile
	}
	if p.MaxMediaBytes <= 0 {
		p.MaxMediaBytes = DefaultMaxMediaBytes
	}
	return nil
}

// Session Ã© o metadado da sessÃ£o. O estado criptogrÃ¡fico do protocolo fica no
// sqlstore (tabelas whatsmeow_*); aqui sÃ³ vive o que Ã© nosso.
type Session struct {
	ID          string
	TenantID    string
	OwnerUserID string
	Name        string
	Phone       string
	// DeviceJID Ã© o vÃ­nculo com o registro do sqlstore. Fica vazio atÃ© o
	// pareamento concluir, porque o device sÃ³ ganha JID nesse momento.
	DeviceJID   string
	Status      Status
	LastError   string
	ConnectedAt *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time

	// PolÃ­tica de conteÃºdo desta sessÃ£o (ver Policy).
	ReceiveGroups      bool
	ReceiveNewsletters bool
	ReceiveBroadcast   bool
	MediaProfile       string
	MaxMediaBytes      int64
}

// Policy devolve a polÃ­tica efetiva da sessÃ£o.
func (s Session) Policy() Policy {
	p := Policy{
		ReceiveGroups:      s.ReceiveGroups,
		ReceiveNewsletters: s.ReceiveNewsletters,
		ReceiveBroadcast:   s.ReceiveBroadcast,
		MediaProfile:       s.MediaProfile,
		MaxMediaBytes:      s.MaxMediaBytes,
	}
	_ = p.Normalize()
	return p
}

// Normalize valida e limpa os campos de entrada antes de persistir.
func (s *Session) Normalize() error {
	s.TenantID = strings.TrimSpace(s.TenantID)
	s.OwnerUserID = strings.TrimSpace(s.OwnerUserID)
	s.Name = strings.TrimSpace(s.Name)

	if s.TenantID == "" {
		return ErrEmptyTenant
	}
	if s.Name == "" {
		return ErrEmptyName
	}
	if len([]rune(s.Name)) > maxNameLength {
		return fmt.Errorf("%w: recebido %d", ErrNameTooLong, len([]rune(s.Name)))
	}
	return nil
}
