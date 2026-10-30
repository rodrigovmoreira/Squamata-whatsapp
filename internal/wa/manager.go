// Package wa mantém os clients do whatsmeow: um por sessão, todos no mesmo
// processo e no mesmo arquivo SQLite. É a máquina de estados do pareamento.
package wa

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	waEvents "go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"

	svcEvents "github.com/rodrigovmoreira/Squamata-whatsapp/internal/events"
	"github.com/rodrigovmoreira/Squamata-whatsapp/internal/labels"
	"github.com/rodrigovmoreira/Squamata-whatsapp/internal/sessions"
)

const (
	// qrTTL estima a validade de um QR: o WhatsApp rotaciona o payload em
	// torno de 20 s e emite um novo evento a cada rotação.
	qrTTL = 20 * time.Second
	// pairingWindow é o tempo total que uma sessão fica esperando leitura,
	// igual ao comportamento atual do Calango-bot (120 s).
	pairingWindow = 120 * time.Second
	// dbTimeout limita as escritas feitas de dentro dos handlers de evento.
	dbTimeout = 10 * time.Second
	// maxBootAttempts é quantas vezes a reconexão de boot tenta antes de
	// desistir e deixar a sessão em erro (o operador dá connect manual).
	maxBootAttempts = 4
)

// ErrNoQRCode indica que não há QR válido no momento (ainda não gerado ou já
// rotacionado). O bot deve tratar como "aguarde", não como falha.
var ErrNoQRCode = errors.New("QR code não disponível")

// QRCode é o que o painel precisa para parear.
type QRCode struct {
	Payload   string
	Image     string
	ExpiresAt time.Time
}

type Manager struct {
	container *sqlstore.Container
	repo      *sessions.Repo
	labelRepo *labels.Repo
	sink      svcEvents.Sink
	logger    *slog.Logger

	mu      sync.RWMutex
	clients map[string]*managed
	// mediaSem limita downloads simultâneos de mídia (D1: 4).
	mediaSem chan struct{}
	// sentMu protege o registro de IDs enviados por nós. Serve para não
	// devolver como `message.sent` o eco da própria mensagem enviada pela API
	// (senão o CRM duplica cada resposta da IA).
	sentMu sync.Mutex
	sent   map[string]time.Time
}

// sentTTL é quanto tempo um ID enviado por nós fica registrado. Um eco chega
// em segundos; 5 minutos é folga suficiente.
const sentTTL = 5 * time.Minute

type managed struct {
	tenantID  string
	sessionID string
	client    *whatsmeow.Client
	device    *store.Device
	// policy é a configuração de conteúdo desta sessão (grupos, mídia).
	policy sessions.Policy

	mu        sync.Mutex
	status    sessions.Status
	qrPayload string
	qrImage   string
	qrExpires time.Time
	pairing   *time.Timer
}

func New(container *sqlstore.Container, repo *sessions.Repo, labelRepo *labels.Repo, sink svcEvents.Sink, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	if sink == nil {
		sink = svcEvents.NewDispatcher("", "", nil, logger)
	}
	return &Manager{
		container: container,
		repo:      repo,
		labelRepo: labelRepo,
		sink:      sink,
		logger:    logger,
		clients:   make(map[string]*managed),
		mediaSem:  make(chan struct{}, mediaSlots),
		sent:      make(map[string]time.Time),
	}
}

// Start reconecta no boot todas as sessões já pareadas (equivale ao
// restoreActiveSessions do bot). Sessões que falharem ficam em erro e podem
// ser religadas por connect — nunca reiniciamos em loop infinito.
func (m *Manager) Start(ctx context.Context) {
	paired, err := m.repo.ListPaired(ctx)
	if err != nil {
		m.logger.Error("falha ao listar sessões para reconectar", "erro", err)
		return
	}
	if len(paired) == 0 {
		m.logger.Info("nenhuma sessão pareada para reconectar")
		return
	}

	for _, s := range paired {
		go m.connectWithRetry(ctx, s.TenantID, s.ID)
	}
}

func (m *Manager) connectWithRetry(ctx context.Context, tenantID, sessionID string) {
	backoff := time.Second
	for attempt := 1; attempt <= maxBootAttempts; attempt++ {
		if ctx.Err() != nil {
			return
		}
		if _, err := m.Connect(ctx, tenantID, sessionID); err == nil {
			m.logger.Info("sessão reconectada no boot", "sessionId", sessionID)
			return
		} else {
			m.logger.Warn("falha ao reconectar sessão",
				"sessionId", sessionID, "tentativa", attempt, "erro", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > 60*time.Second {
			backoff = 60 * time.Second
		}
	}
}

// Shutdown desliga os clients sem invalidar o pareamento: o device continua no
// sqlstore e volta a conectar no próximo boot.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	pending := m.clients
	m.clients = make(map[string]*managed)
	m.mu.Unlock()

	for _, st := range pending {
		st.stopPairing()
		st.client.Disconnect()
	}
}

// Connect é idempotente: chamar de novo numa sessão ativa devolve o estado
// atual sem criar um segundo client para o mesmo número.
func (m *Manager) Connect(ctx context.Context, tenantID, sessionID string) (sessions.Session, error) {
	current, err := m.repo.Get(ctx, tenantID, sessionID)
	if err != nil {
		return sessions.Session{}, err
	}

	if existing := m.lookup(sessionID); existing != nil {
		if existing.getStatus().Active() {
			return current, nil
		}
		existing.stopPairing()
		existing.client.Disconnect()
		m.forget(sessionID)
	}

	device, err := m.deviceFor(ctx, current)
	if err != nil {
		return sessions.Session{}, err
	}

	client := whatsmeow.NewClient(device, waLog.Stdout("whatsmeow-"+shortID(sessionID), "WARN", false))
	st := &managed{
		tenantID:  tenantID,
		sessionID: sessionID,
		client:    client,
		device:    device,
		policy:    current.Policy(),
		status:    sessions.StatusInitializing,
	}
	client.AddEventHandler(func(raw any) { m.handleEvent(st, raw) })

	m.mu.Lock()
	m.clients[sessionID] = st
	m.mu.Unlock()

	if err := m.writeStatus(ctx, st, sessions.StatusInitializing, ""); err != nil {
		m.forget(sessionID)
		client.Disconnect()
		return sessions.Session{}, err
	}

	if err := client.Connect(); err != nil {
		m.forget(sessionID)
		client.Disconnect()
		m.fail(st, "falha ao conectar ao WhatsApp: "+err.Error())
		return sessions.Session{}, fmt.Errorf("conectar ao WhatsApp: %w", err)
	}

	// Sem device pareado não há o que reconectar: o fluxo é QR, com janela.
	if device.ID == nil {
		m.startPairingWindow(st)
	}

	return m.repo.Get(ctx, tenantID, sessionID)
}

// Disconnect para o client mas mantém o pareamento (device no sqlstore), então
// o próximo connect não pede QR novamente.
func (m *Manager) Disconnect(ctx context.Context, tenantID, sessionID string) (sessions.Session, error) {
	if _, err := m.repo.Get(ctx, tenantID, sessionID); err != nil {
		return sessions.Session{}, err
	}

	st := m.lookup(sessionID)
	if st == nil {
		// Processo reiniciou: não há client em memória, só normalizamos o estado.
		if err := m.patch(ctx, tenantID, sessionID, sessions.RuntimePatch{
			Status:      statusPtr(sessions.StatusDisconnected),
			LastError:   strPtr(""),
			ClearConnAt: true,
		}); err != nil {
			return sessions.Session{}, err
		}
		return m.repo.Get(ctx, tenantID, sessionID)
	}

	_ = m.writeStatus(ctx, st, sessions.StatusDisconnecting, "")
	st.stopPairing()
	client := st.client
	m.forget(sessionID)
	client.Disconnect()

	if err := m.patch(ctx, tenantID, sessionID, sessions.RuntimePatch{
		Status:      statusPtr(sessions.StatusDisconnected),
		LastError:   strPtr(""),
		ClearConnAt: true,
	}); err != nil {
		return sessions.Session{}, err
	}
	st.setStatus(sessions.StatusDisconnected)
	m.emitStatus(st, sessions.StatusDisconnected, "")

	return m.repo.Get(ctx, tenantID, sessionID)
}

// Delete faz logout (invalida no celular), remove o device do sqlstore e apaga
// o registro da sessão. É o único caminho que descarta o device — em erro de
// autenticação o device é preservado (decisão D6).
func (m *Manager) Delete(ctx context.Context, tenantID, sessionID string) error {
	current, err := m.repo.Get(ctx, tenantID, sessionID)
	if err != nil {
		return err
	}

	if st := m.lookup(sessionID); st != nil {
		m.forget(sessionID)
		st.stopPairing()
		if st.client.IsLoggedIn() {
			logoutCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if err := st.client.Logout(logoutCtx); err != nil {
				m.logger.Warn("logout falhou; seguindo com a remoção", "sessionId", sessionID, "erro", err)
			}
			cancel()
		}
		st.client.Disconnect()
	}

	if current.DeviceJID != "" {
		if err := m.deleteDevice(ctx, current.DeviceJID); err != nil {
			m.logger.Warn("não foi possível remover o device do sqlstore",
				"sessionId", sessionID, "deviceJid", current.DeviceJID, "erro", err)
		}
	}

	return m.repo.Delete(ctx, tenantID, sessionID)
}

func (m *Manager) deleteDevice(ctx context.Context, rawJID string) error {
	jid, err := types.ParseJID(rawJID)
	if err != nil {
		return fmt.Errorf("device_jid inválido %q: %w", rawJID, err)
	}
	device, err := m.container.GetDevice(ctx, jid)
	if err != nil {
		return fmt.Errorf("buscar device: %w", err)
	}
	if device == nil {
		return nil
	}
	return m.container.DeleteDevice(ctx, device)
}

func (m *Manager) QRCode(tenantID, sessionID string) (QRCode, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()

	if _, err := m.repo.Get(ctx, tenantID, sessionID); err != nil {
		return QRCode{}, err
	}

	st := m.lookup(sessionID)
	if st == nil {
		return QRCode{}, ErrNoQRCode
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	if st.qrPayload == "" || time.Now().After(st.qrExpires) {
		return QRCode{}, ErrNoQRCode
	}
	return QRCode{Payload: st.qrPayload, Image: st.qrImage, ExpiresAt: st.qrExpires}, nil
}

func (m *Manager) deviceFor(ctx context.Context, s sessions.Session) (*store.Device, error) {
	if s.DeviceJID == "" {
		// Device novo: ainda não existe no banco (o sqlstore só persiste
		// depois do pareamento, quando o JID é conhecido).
		return m.container.NewDevice(), nil
	}

	jid, err := types.ParseJID(s.DeviceJID)
	if err != nil {
		return nil, fmt.Errorf("device_jid inválido %q: %w", s.DeviceJID, err)
	}
	device, err := m.container.GetDevice(ctx, jid)
	if err != nil {
		return nil, fmt.Errorf("buscar device %s: %w", s.DeviceJID, err)
	}
	if device == nil {
		return nil, fmt.Errorf("device %s não existe no sqlstore (sessão inconsistente)", s.DeviceJID)
	}
	return device, nil
}

func (m *Manager) handleEvent(st *managed, raw any) {
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()

	switch evt := raw.(type) {
	case *waEvents.QR:
		if len(evt.Codes) == 0 {
			return
		}
		m.onQR(ctx, st, evt.Codes[0])

	case *waEvents.PairSuccess:
		m.onPaired(ctx, st, evt)

	case *waEvents.PairError:
		m.fail(st, "falha ao concluir o pareamento: "+evt.Error.Error())

	case *waEvents.Connected:
		m.onConnected(ctx, st)

	case *waEvents.Disconnected:
		m.onDisconnected(ctx, st)

	case *waEvents.StreamReplaced:
		m.onPermanent(st, "conexão substituída: o número foi aberto em outro aparelho")

	case *waEvents.LoggedOut:
		m.onPermanent(st, "conexão encerrada pelo celular; será preciso parear de novo")

	case *waEvents.KeepAliveTimeout:
		m.logger.Warn("keepalive do WhatsApp expirou",
			"sessionId", st.sessionID, "falhas", evt.ErrorCount)

	case *waEvents.Message:
		m.onMessage(ctx, st, evt)

	case *waEvents.LabelEdit, *waEvents.LabelAssociationChat:
		m.applyLabelEvents(ctx, st, raw)

	case *waEvents.HistorySync:
		// Descartado por decisão (D5): o histórico do produto vive no Mongo.
		return

	default:
		return
	}
}

func (m *Manager) onQR(ctx context.Context, st *managed, payload string) {
	image, err := qrDataURI(payload)
	if err != nil {
		m.logger.Error("falha ao gerar imagem do QR", "sessionId", st.sessionID, "erro", err)
	}

	expires := time.Now().UTC().Add(qrTTL)
	st.mu.Lock()
	st.qrPayload = payload
	st.qrImage = image
	st.qrExpires = expires
	st.mu.Unlock()

	if err := m.writeStatus(ctx, st, sessions.StatusQRCode, ""); err != nil {
		return
	}

	m.sink.Emit(svcEvents.Event{
		Event:     svcEvents.SessionQR,
		TenantID:  st.tenantID,
		SessionID: st.sessionID,
		QR:        payload,
		Image:     image,
		ExpiresAt: &expires,
	})
}

func (m *Manager) onPaired(ctx context.Context, st *managed, evt *waEvents.PairSuccess) {
	st.stopPairing()

	deviceJID := evt.ID.String()
	phone := evt.ID.User
	if err := m.patch(ctx, st.tenantID, st.sessionID, sessions.RuntimePatch{
		DeviceJID: &deviceJID,
		Phone:     &phone,
		Status:    statusPtr(sessions.StatusAuthenticated),
		LastError: strPtr(""),
	}); err != nil {
		return
	}

	st.setStatus(sessions.StatusAuthenticated)
	st.clearQR()
	m.emitStatus(st, sessions.StatusAuthenticated, "")
	m.logger.Info("sessão pareada", "sessionId", st.sessionID, "telefone", phone)
}

func (m *Manager) onConnected(ctx context.Context, st *managed) {
	st.stopPairing()
	now := time.Now().UTC()

	patch := sessions.RuntimePatch{
		Status:      statusPtr(sessions.StatusReady),
		LastError:   strPtr(""),
		ConnectedAt: &now,
	}
	if st.device.ID != nil {
		phone := st.device.ID.User
		patch.Phone = &phone
	}
	if err := m.patch(ctx, st.tenantID, st.sessionID, patch); err != nil {
		return
	}

	st.setStatus(sessions.StatusReady)
	st.clearQR()
	m.emitStatus(st, sessions.StatusReady, "")
}

// onDisconnected trata queda transitória. O whatsmeow já reconecta sozinho
// (EnableAutoReconnect), então aqui só refletimos o estado para o painel.
func (m *Manager) onDisconnected(ctx context.Context, st *managed) {
	switch st.getStatus() {
	case sessions.StatusReady, sessions.StatusAuthenticated:
		_ = m.writeStatus(ctx, st, sessions.StatusInitializing, "")
	default:
		// Já estávamos em erro/qrcode: nada novo a informar.
	}
}

// onPermanent cobre os eventos que o whatsmeow não reconecta: o aparelho foi
// removido pelo celular ou outra sessão assumiu as mesmas chaves.
func (m *Manager) onPermanent(st *managed, message string) {
	st.stopPairing()
	st.client.Disconnect()
	m.forget(st.sessionID)
	m.fail(st, message)
}

func (m *Manager) startPairingWindow(st *managed) {
	st.stopPairing()

	st.mu.Lock()
	st.pairing = time.AfterFunc(pairingWindow, func() {
		switch st.getStatus() {
		case sessions.StatusQRCode, sessions.StatusInitializing:
		default:
			return
		}
		m.logger.Warn("janela de pareamento expirada", "sessionId", st.sessionID)
		st.client.Disconnect()
		m.forget(st.sessionID)
		m.failAs(st, sessions.StatusDisconnected, "tempo limite para leitura do QR excedido")
	})
	st.mu.Unlock()
}

func (m *Manager) fail(st *managed, message string) {
	m.failAs(st, sessions.StatusError, message)
}

func (m *Manager) failAs(st *managed, status sessions.Status, message string) {
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()

	if err := m.patch(ctx, st.tenantID, st.sessionID, sessions.RuntimePatch{
		Status:      statusPtr(status),
		LastError:   &message,
		ClearConnAt: true,
	}); err != nil {
		return
	}

	st.setStatus(status)
	st.clearQR()
	m.emitStatus(st, status, message)
	m.sink.Emit(svcEvents.Event{
		Event:     svcEvents.SessionAuthFail,
		TenantID:  st.tenantID,
		SessionID: st.sessionID,
		Status:    string(status),
		Message:   message,
	})
}

func (m *Manager) writeStatus(ctx context.Context, st *managed, status sessions.Status, lastError string) error {
	if err := m.patch(ctx, st.tenantID, st.sessionID, sessions.RuntimePatch{
		Status:    statusPtr(status),
		LastError: &lastError,
	}); err != nil {
		return err
	}
	st.setStatus(status)
	m.emitStatus(st, status, lastError)
	return nil
}

func (m *Manager) patch(ctx context.Context, tenantID, sessionID string, patch sessions.RuntimePatch) error {
	writeCtx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()

	if err := m.repo.UpdateRuntime(writeCtx, tenantID, sessionID, patch); err != nil {
		m.logger.Error("falha ao gravar estado da sessão",
			"sessionId", sessionID, "erro", err)
		return err
	}
	return nil
}

func (m *Manager) emitStatus(st *managed, status sessions.Status, message string) {
	m.sink.Emit(svcEvents.Event{
		Event:     svcEvents.SessionStatus,
		TenantID:  st.tenantID,
		SessionID: st.sessionID,
		Status:    string(status),
		Message:   message,
	})
}

// rememberSent registra que nós enviamos esta mensagem, para descartar o eco.
func (m *Manager) rememberSent(sessionID, messageID string) {
	if messageID == "" {
		return
	}
	key := sessionID + "|" + messageID

	m.sentMu.Lock()
	defer m.sentMu.Unlock()
	m.sent[key] = time.Now()
	m.pruneSentLocked()
}

// consumeSent devolve true (e esquece) se o ID foi enviado por nós.
func (m *Manager) consumeSent(sessionID, messageID string) bool {
	if messageID == "" {
		return false
	}
	key := sessionID + "|" + messageID

	m.sentMu.Lock()
	defer m.sentMu.Unlock()

	if _, ok := m.sent[key]; !ok {
		return false
	}
	delete(m.sent, key)
	return true
}

func (m *Manager) pruneSentLocked() {
	cutoff := time.Now().Add(-sentTTL)
	for key, at := range m.sent {
		if at.Before(cutoff) {
			delete(m.sent, key)
		}
	}
}

func (m *Manager) lookup(sessionID string) *managed {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.clients[sessionID]
}

func (m *Manager) forget(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.clients, sessionID)
}

func (st *managed) setStatus(status sessions.Status) {
	st.mu.Lock()
	st.status = status
	st.mu.Unlock()
}

func (st *managed) getStatus() sessions.Status {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.status
}

func (st *managed) clearQR() {
	st.mu.Lock()
	st.qrPayload = ""
	st.qrImage = ""
	st.qrExpires = time.Time{}
	st.mu.Unlock()
}

func (st *managed) stopPairing() {
	st.mu.Lock()
	if st.pairing != nil {
		st.pairing.Stop()
		st.pairing = nil
	}
	st.mu.Unlock()
}

func statusPtr(s sessions.Status) *sessions.Status { return &s }
func strPtr(s string) *string                      { return &s }

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func nowMillis() int64 {
	return time.Now().UnixMilli()
}
