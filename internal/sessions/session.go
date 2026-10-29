package sessions

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Status é o estado da sessão. Os valores são exatamente os que o painel do
// Calango-bot já recebe hoje (getSessionStatus), para não quebrar o frontend.
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

// Active indica que existe client vivo para a sessão. Usado para que connect
// seja idempotente em vez de criar um segundo client para o mesmo número.
func (s Status) Active() bool {
	switch s {
	case StatusInitializing, StatusQRCode, StatusAuthenticated, StatusReady, StatusDisconnecting:
		return true
	}
	return false
}

var (
	ErrNotFound      = errors.New("sessão não encontrada")
	ErrEmptyTenant   = errors.New("tenantId é obrigatório")
	ErrEmptyName     = errors.New("nome da sessão é obrigatório")
	ErrNameTooLong   = errors.New("nome da sessão deve ter no máximo 80 caracteres")
	ErrInvalidStatus = errors.New("status de sessão inválido")
)

const maxNameLength = 80

// Session é o metadado da sessão. O estado criptográfico do protocolo fica no
// sqlstore (tabelas whatsmeow_*); aqui só vive o que é nosso.
type Session struct {
	ID          string
	TenantID    string
	OwnerUserID string
	Name        string
	Phone       string
	// DeviceJID é o vínculo com o registro do sqlstore. Fica vazio até o
	// pareamento concluir, porque o device só ganha JID nesse momento.
	DeviceJID   string
	Status      Status
	LastError   string
	ConnectedAt *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
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
