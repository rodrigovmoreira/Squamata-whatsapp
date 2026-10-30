package wa

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/types"
	waEvents "go.mau.fi/whatsmeow/types/events"

	"github.com/rodrigovmoreira/Squamata-whatsapp/internal/labels"
	"github.com/rodrigovmoreira/Squamata-whatsapp/internal/sessions"
)

// maxContactsSync limita o payload de uma sincronização (o painel não precisa
// de mais que isso por vez e a resposta vira JSON).
const maxContactsSync = 5000

var (
	ErrLabelNotFound = errors.New("etiqueta não encontrada")
	ErrEmptyLabel    = errors.New("nome da etiqueta é obrigatório")
)

// Contact é o que o bot precisa para atualizar o CRM.
type Contact struct {
	JID      string
	Phone    string
	Name     string
	PushName string
}

// SyncContacts devolve os contatos conhecidos pelo WhatsApp.
// Substitui o scraping via `pupPage` do wwebjsService.
func (m *Manager) SyncContacts(ctx context.Context, tenantID, sessionID string) ([]Contact, bool, error) {
	st, err := m.readyClient(tenantID, sessionID)
	if err != nil {
		return nil, false, err
	}

	all, err := st.client.Store.Contacts.GetAllContacts(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("listar contatos do WhatsApp: %w", err)
	}

	contacts := make([]Contact, 0, len(all))
	truncated := false
	for jid, info := range all {
		if jid.Server != types.DefaultUserServer {
			continue
		}
		// Iron Gate herdado: números plausíveis apenas.
		if len(digitsOnly(jid.User)) == 0 || len(digitsOnly(jid.User)) > maxIDDigits {
			continue
		}
		if len(contacts) >= maxContactsSync {
			truncated = true
			break
		}

		name := strings.TrimSpace(info.FullName)
		if name == "" {
			name = strings.TrimSpace(info.FirstName)
		}
		contacts = append(contacts, Contact{
			JID:      jid.String(),
			Phone:    jid.User,
			Name:     name,
			PushName: info.PushName,
		})
	}
	return contacts, truncated, nil
}

// ListLabels devolve o espelho local das etiquetas.
func (m *Manager) ListLabels(ctx context.Context, tenantID, sessionID string) ([]labels.Label, error) {
	if _, err := m.readyClient(tenantID, sessionID); err != nil {
		return nil, err
	}
	return m.labelRepo.List(ctx, sessionID)
}

// UpsertLabel cria ou edita uma etiqueta. `labelID` vazio cria uma nova
// (o id é gerado localmente, como o WhatsApp faz).
func (m *Manager) UpsertLabel(ctx context.Context, tenantID, sessionID, labelID, name string, color int32) (labels.Label, error) {
	if strings.TrimSpace(name) == "" {
		return labels.Label{}, ErrEmptyLabel
	}

	st, err := m.readyClient(tenantID, sessionID)
	if err != nil {
		return labels.Label{}, err
	}

	if labelID == "" {
		labelID = newLabelID()
	}

	patch := appstate.BuildLabelEdit(labelID, name, color, false)
	if err := st.client.SendAppState(ctx, patch); err != nil {
		return labels.Label{}, fmt.Errorf("enviar edição da etiqueta: %w", err)
	}

	label := labels.Label{LabelID: labelID, Name: name, Color: color}
	if err := m.labelRepo.Upsert(ctx, sessionID, label); err != nil {
		return labels.Label{}, err
	}
	return label, nil
}

func (m *Manager) DeleteLabel(ctx context.Context, tenantID, sessionID, labelID string) error {
	st, err := m.readyClient(tenantID, sessionID)
	if err != nil {
		return err
	}

	patch := appstate.BuildLabelEdit(labelID, "", 0, true)
	if err := st.client.SendAppState(ctx, patch); err != nil {
		return fmt.Errorf("enviar exclusão da etiqueta: %w", err)
	}
	return m.labelRepo.Delete(ctx, sessionID, labelID)
}

// SetChatLabels alinha as etiquetas de um chat com a lista desejada.
func (m *Manager) SetChatLabels(ctx context.Context, tenantID, sessionID, chatJID string, labelIDs []string) error {
	st, err := m.readyClient(tenantID, sessionID)
	if err != nil {
		return err
	}

	jid, err := types.ParseJID(chatJID)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidNumber, chatJID)
	}

	current, err := m.labelRepo.LabelsByChat(ctx, sessionID, jid.String())
	if err != nil {
		return err
	}

	wanted := make(map[string]bool, len(labelIDs))
	for _, id := range labelIDs {
		wanted[id] = true
	}
	existing := make(map[string]bool, len(current))
	for _, id := range current {
		existing[id] = true
	}

	for id := range wanted {
		if existing[id] {
			continue
		}
		if err := st.client.SendAppState(ctx, appstate.BuildLabelChat(jid, id, true)); err != nil {
			return fmt.Errorf("aplicar etiqueta %s: %w", id, err)
		}
		if err := m.labelRepo.SetChatLabel(ctx, sessionID, id, jid.String(), true); err != nil {
			return err
		}
	}
	for id := range existing {
		if wanted[id] {
			continue
		}
		if err := st.client.SendAppState(ctx, appstate.BuildLabelChat(jid, id, false)); err != nil {
			return fmt.Errorf("remover etiqueta %s: %w", id, err)
		}
		if err := m.labelRepo.SetChatLabel(ctx, sessionID, id, jid.String(), false); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) LabelChats(ctx context.Context, tenantID, sessionID, labelID string) ([]string, error) {
	if _, err := m.readyClient(tenantID, sessionID); err != nil {
		return nil, err
	}
	return m.labelRepo.ChatsByLabel(ctx, sessionID, labelID)
}

// readyClient centraliza a checagem "sessão do tenant existe e está conectada".
func (m *Manager) readyClient(tenantID, sessionID string) (*managed, error) {
	st := m.lookup(sessionID)
	if st == nil {
		if _, repoErr := m.repo.Get(context.Background(), tenantID, sessionID); repoErr != nil {
			return nil, repoErr
		}
		return nil, ErrSessionNotReady
	}
	if st.tenantID != tenantID {
		return nil, sessions.ErrNotFound
	}
	if !st.client.IsConnected() || !st.client.IsLoggedIn() {
		return nil, ErrSessionNotReady
	}
	return st, nil
}

// applyLabelEvents mantém o espelho alinhado quando o celular mexe nas etiquetas.
func (m *Manager) applyLabelEvents(ctx context.Context, st *managed, raw any) {
	switch evt := raw.(type) {
	case *waEvents.LabelEdit:
		if evt.Action == nil {
			return
		}
		label := labels.Label{
			LabelID: evt.LabelID,
			Name:    evt.Action.GetName(),
			Color:   evt.Action.GetColor(),
			Deleted: evt.Action.GetDeleted(),
		}
		if label.Deleted {
			_ = m.labelRepo.Delete(ctx, st.sessionID, label.LabelID)
			return
		}
		_ = m.labelRepo.Upsert(ctx, st.sessionID, label)

	case *waEvents.LabelAssociationChat:
		if evt.Action == nil {
			return
		}
		labeled := evt.Action.GetLabeled()
		_ = m.labelRepo.SetChatLabel(ctx, st.sessionID, evt.LabelID, evt.JID.String(), labeled)
	}
}

func newLabelID() string {
	// O WhatsApp usa ids numéricos curtos para etiquetas; usar o timestamp
	// em milissegundos mantém o formato e evita colisão entre sessões.
	return fmt.Sprintf("%d", nowMillis())
}
