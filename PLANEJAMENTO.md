# Planejamento — `squamata-whatsapp`

- **Status:** rascunho para revisão (nada publicado no GitHub ainda)
- **Data:** 2026-10-29
- **Objetivo do serviço:** manter a conexão de WhatsApp do Calango-bot funcionando **exatamente como funciona hoje** com Node + `whatsapp-web.js`, sem Chromium.
- **Decisões que sustentam este plano:** ADR 002 (contrato de API, repo `Calango-docs`), ADR 003 (camadas e SPOF), ADR 005 (SQLite/`sqlstore`), Roadmap 2 (migração dos múltiplos WhatsApps, repo `Calango-bot`).

---

## 1. O que "funcionar como funcionava" significa

Hoje o `Calango-bot` fala com o WhatsApp **dentro do mesmo processo Node**, através de `backend/services/wwebjsService.js` (953 linhas). Esse arquivo concentra cinco responsabilidades que, no novo desenho, são do serviço Go:

| # | Responsabilidade | Hoje (Node) |
| :-- | :--- | :--- |
| 1 | Ciclo de vida da sessão (iniciar, QR, autenticar, cair, reconectar) | `wwebjsService.js` |
| 2 | Envio de texto, imagem e presença | `wwebjsService.js` |
| 3 | Recebimento de mensagem (entrada e `fromMe`) | eventos `message` / `message_create` |
| 4 | Labels (app state) | `wwebjsService.js` + `tagService.js` |
| 5 | Lista de contatos | `contactController.js` (via `pupPage`/`getChats`) |

**A fronteira nova:** o Calango-bot deixa de saber o que é WhatsApp. Ele conhece **sessão** e **mensagem** (conceitos do domínio dele) através de um único módulo adaptador (`backend/services/whatsapp/client.js`).

O que **não** migra: buffer de 11 s, anti-spam, handover, filtros de audiência/horário, IA, `saveMessage`, funil, agenda. Nada disso é WhatsApp — continua no Node, intacto.

### Fora de escopo (declarado)

- Chamadas de voz/vídeo (o `whatsmeow` não faz).
- Broadcast list (o WhatsApp Web também não faz) — campanha continua 1:1, que é o que o `campaignScheduler` já faz.
- Grupos, canais e status: **bloqueados por regra**, como hoje (`@g.us`, `@newsletter`, `status@broadcast`).
- Interface gráfica: o painel continua sendo o do Calango-bot.
- Multi-réplica do serviço: single-node por definição (ADR 005).

---

## 2. Matriz de paridade

Cada linha é um comportamento que precisa existir no fim, com a evidência de onde ele está hoje.

| Comportamento atual | Evidência | No serviço Go | Fase |
| :--- | :--- | :--- | :--- |
| Uma sessão por empresa (`businessId`) | `wwebjsService.js:112` | Sessão com `tenantId` + `sessionId` (UUID). O bot guarda o `sessionId` opaco | F1 |
| Status `initializing → qrcode → authenticated → ready → disconnected / error / disconnecting` | `:945` `getSessionStatus` | Mesmo enum, exposto em `GET /sessions/:id` | F1 |
| QR enviado ao painel | `:235` `client.on('qr')` → socket `wwebjs_qr`; frontend desenha com `qrcode.react` (`ConnectionTab.jsx:176`) | **Go gera o QR** (payload + PNG) — `GET .../qr` e evento `session.qr`; painel só exibe | F2 |
| Timeout de 120 s esperando leitura do QR | `:173-198` | `qrcode` + timer → `disconnected`; valor configurável | F2 |
| `auth_failure` → encerra tudo e pede novo QR | `:280` | Evento `session.auth_failure`; **não** apaga o device automaticamente (ver D6) | F2 |
| Queda de conexão → sessão morre | `:343` `client.on('disconnected')` | Reconexão automática com backoff; logout só por chamada explícita | F2 |
| Detecção de sessão "zumbi" (60 s / 90 s) | `:562`, `:670` | `whatsmeow` entrega `events.Disconnected`/`StreamReplaced`; health periódico só como rede de segurança | F2 |
| Auto-start de sessões no boot | `server.js:250` `restoreActiveSessions` | Serviço reconecta todas as sessões do SQLite no boot | F2 |
| Envio de texto com retorno `true/false` | `:679` `sendWWebJSMessage` | `POST .../messages` → `200` síncrono (D4) | F4 |
| Normalização do número (`55` + `@c.us`) | `:684-710` | Centralizada no Go (parse/format de JID) | F4 |
| Envio de imagem por URL + legenda | `:718` `sendImage` | `POST .../messages/media` | F4 |
| "digitando…" e presença | `:788`, `:775` | `POST .../presence` | F4 |
| Entrada de mensagem (texto/mídia) | `:300` `client.on('message')` | Handler `events.Message` → webhook | F3 |
| Bloqueio de grupo/status/newsletter/ID longo | `:295-315` | Regra no Go antes de emitir evento | F3 |
| Mensagem enviada pelo celular aparece no CRM | `:318` `message_create` (`fromMe`) | Evento `message.sent` no webhook | F3 |
| `@lid` (ID de privacidade) desmascarado para telefone | `messageHandler.js:83`, `contactController.js` | Resolução de LID no Go (conferir API exata na F0) | F3 |
| Mídia recebida em base64 para visão/transcrição | `providerAdapter.js` `adaptWWebJSMessage` | base64 no webhook (só `image`/`audio`, ≤ 8 MB) | F3 |
| Histórico de conversa no painel | `Message`/`ChatMessage` + `GET /api/v1/business/conversations/:contactId/messages` | **não migra** — continua no Mongo; o serviço não é fonte de histórico (D5) | — |
| Labels: listar, criar, editar, excluir, vincular a chat | `wwebjsService.js:827-903`, `tagService.js:79-267` | Endpoints `/labels` + `/chats/:chatId/labels` | F6 |
| Importar labels do WhatsApp para `Tag`/`Contact` | `whatsappController.js:18-130` | `GET .../labels` + `GET .../labels/:id/chats` | F6 |
| Sincronizar lista de contatos | `contactController.js:525-560` | `POST .../contacts/sync` | F5 |

---

## 3. Modelo de domínio do serviço

### 3.1 Sessão

```go
type Session struct {
    ID          string // UUID — é o sessionId que o Calango-bot guarda
    TenantID    string // businessId
    OwnerUserID string // opcional: vazio = sessão do setor/empresa
    Name        string
    Phone       string // preenchido após pareamento
    Status      Status // máquina de estados abaixo
    LastError   string
    ConnectedAt time.Time
    CreatedAt   time.Time
}

// Mesmos valores que o painel já recebe hoje (getSessionStatus), para o
// frontend não precisar mudar: initializing, qrcode, authenticated, ready,
// disconnecting, disconnected, error.
type Status string
```

Duas persistências, no **mesmo** arquivo SQLite:

| Tabela | Dono | Conteúdo |
| :--- | :--- | :--- |
| `whatsmeow_*` | `sqlstore` (biblioteca) | Chaves criptográficas do Signal. **Nunca tocar à mão.** |
| `sessions` | nosso | Metadados acima (nome, tenant, proprietário, status) |

Prefixo próprio nas tabelas para nunca colidir com as do `sqlstore`. Migração própria com `PRAGMA user_version`.

### 3.2 Envelope de mensagem (contrato com o bot)

```json
{
  "event": "message",
  "tenantId": "68f...",
  "sessionId": "3f2c...",
  "messageId": "3EB0...",
  "from": "5511987654321@s.whatsapp.net",
  "rawFrom": "123456789012345@lid",
  "pushName": "Fulano",
  "type": "text | image | audio | video | document | other",
  "text": "bom dia",
  "media": {
    "mime": "image/jpeg",
    "filename": "x.jpg",
    "size": 48213,
    "seconds": null,
    "base64": "..."
  },
  "timestamp": "2026-10-29T13:45:00Z",
  "fromMe": false
}
```

`type`, `text` e `media` são os mesmos conceitos que o `adaptWWebJSMessage` já produz — é isso que permite o `messageHandler` continuar funcionando trocando apenas o adaptador.

---

## 4. Contrato HTTP (MVP)

Escopo do tenant **no caminho**, para que isolamento não dependa de lembrar de filtrar.

```
GET    /api/v1/health

# Sessões
POST   /api/v1/tenants/:tenantId/sessions                 { name, ownerUserId? }
GET    /api/v1/tenants/:tenantId/sessions
GET    /api/v1/tenants/:tenantId/sessions/:id
DELETE /api/v1/tenants/:tenantId/sessions/:id             (logout + remove device)
POST   /api/v1/tenants/:tenantId/sessions/:id/connect     (idempotente)
POST   /api/v1/tenants/:tenantId/sessions/:id/disconnect  (mantém o device)
GET    /api/v1/tenants/:tenantId/sessions/:id/qr          { qr, image, expiresAt }
POST   /api/v1/tenants/:tenantId/sessions/:id/pairing-code { phone } → { code }

# Mensagens
POST   /api/v1/tenants/:tenantId/sessions/:id/messages        { to, text }
POST   /api/v1/tenants/:tenantId/sessions/:id/messages/media  { to, mediaUrl, mime, caption }
POST   /api/v1/tenants/:tenantId/sessions/:id/presence        { to, type: "typing" | "paused" }

# App state
GET    /api/v1/tenants/:tenantId/sessions/:id/labels
POST   /api/v1/tenants/:tenantId/sessions/:id/labels          { name, color }
PATCH  /api/v1/tenants/:tenantId/sessions/:id/labels/:labelId { name?, color? }
DELETE /api/v1/tenants/:tenantId/sessions/:id/labels/:labelId
GET    /api/v1/tenants/:tenantId/sessions/:id/labels/:labelId/chats
PUT    /api/v1/tenants/:tenantId/sessions/:id/chats/:chatId/labels { labelIds: [] }

# Contatos
POST   /api/v1/tenants/:tenantId/sessions/:id/contacts/sync
```

### Regras transversais

| Regra | Detalhe |
| :--- | :--- |
| **Auth** | `Authorization: Bearer <SQUAMATA_INTERNAL_TOKEN>` em **todas** as rotas, exceto `/health`. Ausente/errado → `401`. Comparação em tempo constante. |
| **Erros** | `{ "error": { "code": "SESSION_NOT_CONNECTED", "message": "..." } }`. Códigos estáveis: `SESSION_NOT_FOUND`, `SESSION_ALREADY_CONNECTED`, `SESSION_NOT_CONNECTED`, `NOT_A_BUSINESS_ACCOUNT`, `MEDIA_TOO_LARGE`, `WHATSAPP_REJECTED`. |
| **Idempotência** | `connect`/`disconnect` podem ser chamados N vezes: mesmo estado final, sem erro. `POST .../messages` aceita `Idempotency-Key` opcional (dedup de 5 min). |
| **Timeout de envio** | 30 s; estourado → `504` com código `WHATSAPP_TIMEOUT`. |
| **Síncrono vs. assíncrono** | Envio é síncrono (o bot precisa saber se falhou para o retry da IA) — ver D4. |
| **Mídia** | Bytes apenas de `image`/`audio`, até 8 MB decodificados; acima disso a mensagem segue **sem** bytes (`mediaTooLarge: true`). Corpo do webhook limitado a 12 MB nessa rota. |

---

## 5. Entrada: do WhatsApp para o Calango-bot

```
WhatsApp ──▶ squamata-whatsapp (events.Message / events.Sent / events.Connected …)
                 │ normaliza, resolve @lid, baixa/salva mídia
                 ▼
POST {CALANGO_BOT_WEBHOOK_URL}    (HMAC-SHA256 em X-Signature)
```

| Requisito | Decisão |
| :--- | :--- |
| Assinatura | HMAC-SHA256 do corpo bruto, segredo `SQUAMATA_WEBHOOK_SECRET`. Sem assinatura válida → `401`, **sem processar**. |
| Semântica | **At-least-once** com backoff exponencial (1 s → 2 s → 4 s … até 5 min, teto de 10 tentativas). |
| Ordenação | Não garantida entre tentativas; o bot ordena por `timestamp` e deduplica por `messageId`. |
| Resposta do bot | `202 Accepted` rápido; processamento assíncrono. Falha do bot **não** derruba a sessão. |
| Contrapressão | Fila em memória por sessão (tamanho fixo). Cheia → log de erro e descarte controlado (mensagem continua no histórico do WhatsApp). |
| Destino | `POST /api/v1/webhooks/whatsapp` no Calango-bot (ver D2). |

**Eventos emitidos:** `message`, `message.sent`, `session.status`, `session.qr`, `session.auth_failure`.

**Nunca emitido:** `HistorySync` — descartado por decisão (D5, §9). O CRM não importa conversas do aparelho.

> `message.sent` substitui o `message_create`/`fromMe`: sem ele, mensagens enviadas pelo celular do dono do número deixam de aparecer no CRM — hoje isso já está resolvido e precisa continuar resolvido.

### Mensagem que chega com o bot fora do ar (D8)

O outbox **não é um segundo histórico** — é fila de entrega. A mensagem entra no Mongo quando o bot volta e processa o reenvio, pelo caminho normal (`handleIncomingMessage` → `saveMessage`). O histórico continua tendo uma única fonte.

```
WhatsApp → Go → webhook ──✗ bot fora──▶ outbox (SQLite: payload bruto, tentativas, next_retry_at)
                                            │ bot volta (202)
                                            ▼
                                 handleIncomingMessage → Mongo (timestamp original)
```

Após o `202`, a linha do outbox é removida. Duas exigências no lado do bot, sem as quais o reenvio sai errado:

| Exigência | Por quê |
| :--- | :--- |
| Gravar o **timestamp original** do payload | Hoje `saveMessage` não recebe `timestamp` (`message.js:119-150`) e o schema aplica `Date.now` (`Message.js:41`). Reenviar 3 h depois jogaria a mensagem no fim da conversa, como se tivesse chegado agora |
| **Dedup na entrada do webhook**, antes do buffer/IA | `saveMessage` já deduplica por `waMessageId` (`message.js:136-143`), mas isso roda **depois** da IA — o cliente receberia a mesma resposta duas vezes |

**Limites do outbox:** descarta o registro mais antigo acima de 5 000 linhas ou 7 dias (o que vier primeiro), com log. O aparelho do cliente continua com o histórico dele, então nada se perde de fato.

---

## 6. Saída: do Calango-bot para o WhatsApp

```
Calango-bot (client.js) ──HTTP──▶ squamata-whatsapp ──▶ whatsmeow
```

| Ponto | Regra |
| :--- | :--- |
| Texto | `SendMessage` com retry em falha transitória (2 tentativas, 1 s). Falha definitiva → `502`, bot registra e segue (degradação graciosa). |
| Mídia | O serviço **baixa a URL** (hoje o Node faz isso com `axios`) com timeout de 30 s e teto de 8 MB — o mesmo da entrada; envia com legenda. |
| Presença | `typing` / `paused`; nunca bloqueia o envio da mensagem. |
| Número | Aceitar as duas formas que o bot usa hoje: `11999999999` e `5511999999999`. Normalização no Go, uma única vez. |
| `@lid` | Sempre tentar resolver para telefone antes de chamar a API; se não resolver, usar o `@lid` como destino (o `whatsmeow` aceita). |
| Se a sessão não existe/está caída | `409 SESSION_NOT_CONNECTED` — o bot **não** deve tentar de novo em loop; registra e segue. |

---

## 7. Mídia

| Direção | Hoje | Novo (decidido) |
| :--- | :--- | :--- |
| Entrada | base64 em memória → visão/transcrição, **não persiste** | **base64 no webhook**, sem persistir — mesmo formato que a visão/transcrição já consomem |
| Saída (campanha/IA) | URL (Firebase) → download no Node → base64 → envio | mesma ideia, mas quem baixa é o serviço Go: o bot manda **URL** |

### Por que base64 e não URL (D1)

Persistir mídia exigiria estender o `squamata-upload`, que hoje **só assina upload** — não existe endpoint de leitura nem upload server-to-server. Para o volume real do produto, o ganho não paga o custo.

**Quanto o base64 onera de fato:** +33% de tamanho e 2 a 3 cópias em memória por mensagem em trânsito (bytes → string base64 → corpo JSON; no Node, corpo → string → `Buffer`).

| Cenário | Original | Base64 | Pico estimado por mensagem em trânsito |
| :--- | :--- | :--- | :--- |
| Texto | < 1 KB | — | KB |
| Foto do WhatsApp (comprimida pelo app) | 100–500 KB | 133–667 KB | ~0,5–2 MB |
| Áudio (PTT) | 50–300 KB | 67–400 KB | ~0,3–1,2 MB |
| Vídeo/imagem no teto do WhatsApp | 16 MB | 21,3 MB | **~70–90 MB** |

O último caso é o que realmente pesa — e é evitável: hoje o código baixa mídia de **qualquer** tipo (`msg.hasMedia`, `providerAdapter.js`). Regras que eliminam o problema:

1. **Só baixar bytes de `image` e `audio`** — os dois tipos que o bot realmente processa (`analyzeImage` / `transcribeAudio`). Vídeo, documento e sticker vão apenas como metadados (`type`, `mime`, `size`, `filename`), sem bytes.
2. **Teto de 8 MB decodificados** por mensagem. Acima: a mensagem é entregue **sem** mídia com `mediaTooLarge: true` — nunca falha nem trava a conversa.
3. **Semáforo de 4 downloads simultâneos** no serviço, limitando o pior caso a algumas centenas de MB.
4. **Nada é persistido**: o `Message` do Mongo continua sem bytes de mídia, como hoje — não há crescimento de banco.
5. **Limite de corpo só na rota do webhook** (`express.json({ limit: '12mb' })`, não global). O default do Express é 100 KB e derrubaria toda mídia.

**Quando revisar:** se o painel passar a precisar de histórico de mídia, ou o tráfego interno virar relevante, aí vale o endpoint de leitura no `squamata-upload`. O payload já nasce com `mime`/`size`/`filename`, então a mudança é de campo (`base64` → `url`), não de contrato.

---

## 8. Ciclo de vida da sessão

```
disconnected ──connect──▶ initializing ──qr──▶ qrcode ──leitura──▶ authenticated ──▶ ready
      ▲                         │                  │                                   │
      │                         └──timeout 120s────┤                                   │
      └──────── disconnect ─────────────────────────┴───────────── queda ◀─────────────┘
                                     (backoff de reconexão)
```

`ready` é o único estado em que envio é permitido; `authenticated` é transitório (as credenciais valeram, mas o app state ainda está carregando), como hoje.

| Situação | Comportamento |
| :--- | :--- |
| Boot do serviço | Abre o SQLite, aplica migrações e reconecta **todas** as sessões conhecidas (equivale ao `restoreActiveSessions`). |
| `connect` chamado já conectado | `200` com o status atual — nunca cria client duplicado (a blindagem de `startSession` de hoje). |
| Duas chamadas `connect` simultâneas | Mutex por `sessionId`; a segunda espera e recebe o mesmo resultado. |
| Queda de rede/WhatsApp | `events.Disconnected` → reconexão com backoff (1 s, 2 s, 4 s… teto 60 s, jitter). |
| `StreamReplaced` (abriu em outro aparelho) | Para a reconexão: **não** brigar pelo número. Status `error` + evento `session.auth_failure`. |
| Timeout de QR | `qrcode` → `disconnected`, client descartado, device **mantido** no banco. |
| `disconnect` explícito | `client.Logout()` (invalida no celular) + para a reconexão. |
| `DELETE` da sessão | `Logout()` + remove `device` do `sqlstore` + remove metadados. |
| Shutdown (SIGTERM) | `Disconnect()` de todos os clients, fecha o SQLite, respeitando o `stop_grace_period` do compose. |
| Pareamento por código | `PairPhone` (8 dígitos) — alternativa ao QR, melhora de produto. |

---

## 9. Pareamento: quem gera o QR

**Decidido: o QR é gerado no serviço Go.** Nada de `whatsapp-web.js` ou `qrcode-terminal` sobrevive.

Hoje o backend só repassa a string e o navegador desenha (`wwebjsService.js:235` → `ConnectionTab.jsx:176`):

```
whatsapp-web.js ──socket `wwebjs_qr` com a STRING──▶ React
React: <QRCodeSVG value={qr} size={180} />      ← o navegador desenha
```

No novo desenho, o `whatsmeow` entrega em `events.QR` a **string de pareamento** — ela é a credencial do protocolo e nasce inevitablemente no Go. Sobre o **desenho** da imagem, decisão tomada: o Go também gera o PNG.

```
whatsmeow (events.QR) ──▶ Go: payload + PNG (biblioteca pura Go, sem CGO) ──▶ bot ──▶ React
React: <img src="data:image/png;base64,..." />
```

| Item | Quem gera |
| :--- | :--- |
| Conteúdo do QR (credencial de pareamento) | `whatsmeow` |
| Imagem PNG exibida no painel | serviço Go |
| Exibição | painel (`<img>` no lugar de `QRCodeSVG`) |

`GET .../qr` devolve os dois formatos — imagem para a tela, string para diagnóstico e pareamento manual:

```json
{ "qr": "2@abc...", "image": "data:image/png;base64,iVBOR...", "expiresAt": "2026-10-29T14:47:00Z" }
```

O QR expira (~20 s) e o `whatsmeow` emite um novo. O serviço mantém o último válido em memória e emite `session.qr` a cada rotação; o painel recebe por socket (o bot reemite o webhook) e não precisa de polling. O `GET .../qr` é fallback para aba recarregada no meio do pareamento e responde `409 QR_NOT_AVAILABLE` quando ainda não há QR válido.

> **Formato do payload:** na revisão do `whatsmeow` fixada no `go.mod`, o valor de `events.QR.Codes[0]` vem como link (`https://wa.me/settings/l?...`), não mais como a string `2@...`. O painel **não deve interpretar** o conteúdo — só renderizar. Mudar de versão da biblioteca pode mudar esse formato.

### O que acontece depois da leitura

| Evento | Tratamento |
| :--- | :--- |
| `events.PairSuccess` | Status `authenticated` → `ready`; grava telefone e `pushName` na sessão |
| `events.HistorySync` | **Descartar** — ver D5 abaixo |
| `events.Connected` | Marca `ready` e reinicia o health check |

### Histórico: o WhatsApp não é a fonte da verdade (D5)

Ao parear, o WhatsApp despeja conversas antigas. **Isso é descartado.** O histórico do produto sempre viveu no Mongo:

- `Message` (coleção `ChatMessage`), indexado por `contactId + timestamp`, com `waMessageId` único e esparso para dedup.
- O painel lê em `GET /api/v1/business/conversations/:contactId/messages`.
- A IA monta contexto com `getLastMessages(...)` — 15 mensagens vindas do Mongo, nunca do WhatsApp.

Três motivos para descartar:

1. **Importar reescreveria a verdade.** O que o cliente viu é o que está no Mongo. O dump do celular traz conversas que podem ter sido apagadas no CRM, contatos já desativados e mensagens fora de ordem.
2. **A IA responderia o passado.** Cada mensagem importada passaria pelo `handleIncomingMessage` e poderia disparar resposta automática a uma conversa de dias atrás.
3. **Não há nada a perder ao reparear.** O `stopSession` de hoje limpa **apenas credenciais** (GridFS + pastas `.wwebjs_*`), nunca `Message`/`Contact`. O serviço Go mantém esse comportamento: apaga o `device` do `sqlstore`, jamais o Mongo.

**Consequência aceita:** mensagens trocadas **antes** do pareamento não entram no CRM — igual a hoje. O aparelho continua com o histórico dele; o que não acontece é a cópia para o CRM. Se um dia isso for necessário, é uma feature de importação seletiva, idempotente pelo `waMessageId` e que **nunca** dispara a IA (entraria como histórico, não como mensagem recebida).

> Descartar `events.HistorySync` **não** afeta labels nem contatos: esses vêm do *app state sync*, que continua acontecendo normalmente.

---

## 10. Labels e contatos

| Operação | `whatsmeow` | Observação |
| :--- | :--- | :--- |
| Listar labels | `GetLabels` (app state) | Precisa que o app state esteja sincronizado; validar na F0 |
| Criar/editar/excluir | `PutLabel` / `DeleteLabel` | Exige conta **Business**; se não for, `409 NOT_A_BUSINESS_ACCOUNT` |
| Labels de um chat | `GetChatLabels` / `SetChatLabel` | É o que alimenta `Tag.whatsappId` |
| Lista de contatos | `Store.Contacts.GetAllContacts` + app state | Substitui o scraping por `pupPage`; não devolve "todos os chats" com a mesma semântica — validar cobertura na F0/F5 |

Regra herdada que continua valendo: a `Tag` do Mongo é a fonte da UI; o WhatsApp é espelho. Falha ao falar com o WhatsApp **não** derruba a listagem de tags local.

---

## 11. O que muda no Calango-bot (quando chegar a hora)

> Resumo das mudanças no bot. Inventário completo do resíduo, ordem de execução, migração de dados e critérios de aceite: **§15**.

| Item | Mudança |
| :--- | :--- |
| `services/wwebjsService.js` | **Deletado**; substituído por `services/whatsapp/client.js` |
| `services/UnifiedMongoStore.js`, `.wwebjs_auth/`, `.wwebjs_cache/` | **Deletados** |
| `providerAdapter.js` | `adaptWWebJSMessage` → `adaptSquamataMessage`; `normalizePhone` continua |
| `messageHandler.js` | Recebe payload normalizado; `sendStateTyping` → `presence`; some o `getClientSession` |
| `responseService.js` | Envia por `sessionId` |
| `aiTools.js`, `campaignScheduler.js` | `sendImage` → `messages/media` |
| `tagService.js` | Labels via API |
| `whatsappRoutes.js` | Proxy fino do adaptador (mesmo caminho `/api/v1/whatsapp/*` para o frontend não mudar) |
| `server.js` | Sem `initializeWWebJS`/`restoreActiveSessions`; socket reemite os eventos vindos do serviço |
| `Contact/Message/Appointment/BusinessConfig.whatsappSessionId` | `ObjectId` → `String` (ID externo). Migração **junto** com a virada |
| `services/message.js` | `saveMessage` passa a gravar o **timestamp original** do payload (hoje o schema aplica `Date.now`) |
| `services/whatsapp/inbound.js` | Dedup por `(sessionId, messageId)` **antes** do buffer/IA — senão o reenvio do outbox duplica a resposta |
| Env novas | `SQUAMATA_WHATSAPP_URL`, `SQUAMATA_INTERNAL_TOKEN`, `SQUAMATA_WEBHOOK_SECRET` |

**Sobre os eventos de socket:** hoje o painel escuta `wwebjs_qr` e `wwebjs_status`. Proposta: renomear para `whatsapp_qr`/`whatsapp_status` **na mesma entrega** que muda o frontend (F2), para não sobrar nome de biblioteca morta. O contrato HTTP (`/api/v1/whatsapp/status`) não muda.

---

## 12. Fases e critérios de aceite

| Fase | Entrega | Critério de aceite |
| :--- | :--- | :--- |
| **F0 — Spike** | Número de teste; envio/recebimento; validação de `sqlstore` + `modernc` (já feita); levantar API real de labels, LID e contatos | Mensagem real vai e volta sem Chromium; achados de API registrados |
| **F1 — Esqueleto** | Repo, `/api/v1/health`, store SQLite, tabela `sessions`, auth interna, CRUD de sessão | `go run` sobe nativo; `docker compose up` sobe no servidor; health responde; sessão criada sobrevive a restart |
| **F2 — Pareamento** | QR (payload + PNG), máquina de estados com janela de 120 s, reconexão no boot, eventos assinados. *Pairing code fica para depois da F3 (D10)* | Reiniciar o processo mantém a sessão autenticada; QR chega ao painel; **nenhuma conversa antiga é reprocessada após o pareamento** (D5) |
| **F3 — Entrada** | Handler de mensagens, normalização, mídia, outbox e webhook HMAC (lado serviço). O `inbound.js` no bot é o passo seguinte | Mensagem real chega ao CRM com nome, telefone e texto; imagem chega e é analisada pela visão; reenvio não duplica |
| **F4 — Saída** | Texto, mídia, presença | Resposta da IA sai pelo número novo; campanha envia imagem |
| **F5 — Contatos** | `contacts/sync` | Painel sincroniza contatos sem Puppeteer |
| **F6 — Labels** | CRUD de labels + vínculo a chat | `tagService` cria/edita/exclui label e o painel reflete |
| **F7 — Multi-sessão** | Sessão nomeada por setor **ou** por usuário; `OwnerUserID` | Duas sessões simultâneas, contextos isolados |
| **F8 — Desligamento** | Remover `whatsapp-web.js`/`puppeteer`/`wwebjsService` | `grep -r "wwebjs\|puppeteer\|pupPage" backend/` vazio (fora de `tests/`) |

---

## 13. Testes e observabilidade

| Nível | O que cobre |
| :--- | :--- |
| Unitário Go | Normalização de número/JID, máquina de estados, HMAC, dedup, parser de payload |
| Integração Go | `sqlstore` + `modernc` em arquivo temporário (`t.TempDir()`), migrações, reabertura |
| Contrato | `httptest` batendo nos endpoints com o mesmo JSON que o `client.js` do bot envia |
| Aceite (manual, número real) | Parear → receber → enviar texto → enviar imagem → reiniciar container → continua conectado |
| Observabilidade | Log estruturado com `sessionId`/`tenantId`; `GET /health` com estado do store e contagem de sessões por status |

---

## 14. Riscos

| Risco | Mitigação |
| :--- | :--- |
| `whatsmeow` é protocolo não oficial → risco de banimento | Mesmo risco da biblioteca atual, não é risco novo. Número dedicado de teste; sem disparo em massa agressivo |
| Paridade de labels/contatos pior que o `whatsapp-web.js` | Validar na F0 antes de prometer F5/F6; se faltar cobertura, manter a feature degradada (tags locais continuam funcionando) |
| Mídia grande trafegando em base64 | Só `image`/`audio` e teto de 8 MB; vídeo/documento vão como metadado; semáforo de downloads |
| Webhook perdido (bot fora do ar) | At-least-once + backoff + log; mensagem continua no WhatsApp do usuário |
| Duplicação de mensagem na virada | Dedup por `messageId` dos dois lados; virada só com um dos caminhos ativo |
| Cliente espera ver no CRM conversa antiga do celular | D5: não importamos histórico. Deixar isso explícito no onboarding; importação seletiva é feature futura, não migração |
| Sessão única por tenant no MVP | `BusinessConfig.whatsappSessionId` (string) agora; array na F7 |

### Decisões fechadas

| # | Decisão |
| :-- | :--- |
| **D1** | Mídia entra como **base64** no webhook — só `image`/`audio`, ≤ 8 MB decodificados, sem persistir. Ver §7. |
| **D2** | Webhook em **`/api/v1/webhooks/whatsapp`**: somos nós nas duas pontas, então o contrato é versionado. |
| **D3** | **QR gerado no Go** — payload pelo `whatsmeow` **e** imagem PNG no serviço; o painel só exibe. Ver §9. |
| **D4** | Envio **síncrono**, timeout de 30 s — o bot precisa do resultado para o retry da IA. |
| **D5** | **Histórico é do Mongo.** `events.HistorySync` é descartado ao parear; reparear não apaga `Message`/`Contact`. Ver §9. |
| **D6** | **Sessão inválida se preserva.** `auth_failure`/`StreamReplaced` → status `error`, device mantido no SQLite; apagar só no `DELETE .../sessions/:id`. O repareamento reaproveita o mesmo registro. |
| **D7** | **Só o serviço reconecta no boot.** O bot nunca chama `connect` na inicialização — evita duas conexões brigando pelo mesmo número. |
| **D8** | **Outbox no SQLite** para mensagem que chega com o bot fora do ar; no reenvio a mensagem é gravada no Mongo com o **timestamp original** e dedup **antes** da IA. Ver §5. |
| **D9** | **Uma sessão por empresa** no MVP (`BusinessConfig.whatsappSessionId` como `String`); multi-sessão na F7. |
| **D10** | **Pareamento por código de 8 dígitos depois da F3** — QR resolve a migração; o código é melhoria de onboarding. |
| **D11** | **Eventos de socket renomeados** para `whatsapp_qr`/`whatsapp_status` na F2, junto com a mudança do frontend. |
| **D12** | **Token interno único do ecossistema** (`SQUAMATA_INTERNAL_TOKEN`) no MVP; rotação por serviço depois. |

### Ajustes de implementação (não precisam de decisão)

Nada mais bloqueia a migração. O que resta são parâmetros, ajustáveis direto no código:

- QR: janela de 120 s para leitura (o `whatsmeow` rotaciona o payload a cada ~20 s).
- Envio: timeout de 30 s; reconexão por backoff até 60 s com jitter.
- Mídia: 8 MB decodificados, 4 downloads simultâneos, apenas `image`/`audio`.
- Webhook: 10 tentativas com teto de 5 min; outbox com 5 000 linhas / 7 dias.
- Porta 3006; QR guardado em memória, nunca em disco.

---

## 15. Limpeza do repositório Calango-bot

A limpeza **faz parte da migração**, mas tem ordem obrigatória: nada sai antes de a virada funcionar. Sem número em produção, não há dupla execução (Roadmap 2 §10) — então deletar antes da F4 verde significa ficar sem WhatsApp e sem rollback.

### 15.1 Sai por inteiro

| Item | Tamanho / evidência | Por que sai |
| :--- | :--- | :--- |
| `backend/services/wwebjsService.js` | 953 linhas | Todo o ciclo de sessão agora é do serviço Go |
| `backend/services/UnifiedMongoStore.js` | 122 linhas | Store de credenciais em GridFS; substituído pelo `sqlstore` |
| `backend/tests/mocks/whatsappMock.js` | mock da lib (não usado: "not directly used") | Mock de biblioteca que não existe mais |
| `backend/scripts/verify_tag_structure.js` | script ad-hoc que valida métodos do `wwebjsService` | Coberto por `tests/tag_protection.test.js` |
| `.wwebjs_auth/`, `.wwebjs_cache/`, `*.zip` no host | não versionados (`.gitignore`) | Sobras do Chromium no disco de cada dev/servidor |

### 15.2 Dependências (`backend/package.json`)

| Pacote | Evidência | Ação |
| :--- | :--- | :--- |
| `whatsapp-web.js@^1.34.7` | importado só por `wwebjsService.js` | remover |
| `puppeteer@^19.11.1` | só pelo `whatsapp-web.js` | remover |
| `qrcode@^1.5.4` | **zero importações** (grep em `backend/`) | remover — já é código morto hoje |
| `qrcode-terminal@^0.12.0` | **zero importações** | remover — também já morto |

Comando: `npm uninstall whatsapp-web.js puppeteer qrcode qrcode-terminal` (regenera o lock).

### 15.3 Docker

| Arquivo | Hoje | Depois |
| :--- | :--- | :--- |
| `backend/Dockerfile` | base `ghcr.io/puppeteer/puppeteer:21.5.0`, `PUPPETEER_SKIP_CHROMIUM_DOWNLOAD`, `PUPPETEER_EXECUTABLE_PATH`, `USER pptruser` | base Node enxuta + multi-stage conforme [Architecture.md §5.1](../../Calango-docs/Architecture.md) |
| `backend/.dockerignore` | `.wwebjs_cache/`, `.wwebjs_auth/`, `*.zip`, `**/*.zip` | remover as 4 linhas |

Ganho medido no build: a imagem do backend deixa de carregar Chromium (~1,2 GB) e o `npm ci` para de baixar o bundle do Puppeteer.

### 15.4 Git e ambiente

- `.gitignore`: remover `.wwebjs_cache/`, `.wwebjs_auth/`, `*.zip`, `**/*.zip`.
- `.env.example`: **não** tem `CHROME_BIN` (nada a remover). Acrescentar `SQUAMATA_WHATSAPP_URL`, `SQUAMATA_INTERNAL_TOKEN`, `SQUAMATA_WEBHOOK_SECRET`. O bloco Twilio permanece.

### 15.5 Código que fica e é ajustado

| Arquivo | Ajuste |
| :--- | :--- |
| `server.js:47-52, 254-313, 327` | Sem `initializeWWebJS`, `restoreActiveSessions`, `closeAllSessions`. Socket passa a reemitir eventos do serviço |
| `server.js:237, 240` | Eventos renomeados: `whatsapp_status` / `whatsapp_qr` (D11) |
| `providerAdapter.js:82` | `adaptWWebJSMessage` → `adaptSquamataMessage`; `normalizePhone` continua |
| `messageHandler.js:83, 327` | Sem `getClientSession` (resolver `@lid` passa a vir pronto no payload) |
| `messageHandler.js:169, 195` | `sendStateTyping` → `presence` no adaptador |
| `responseService.js:15` | `provider === 'wwebjs'` → `'squamata'` |
| `contactController.js:246-520` | `fetchChatsWithRetry`, `pupPage.evaluate` (`:265`), `waitForFunction` (`:333`) e fallback `getChats()` → `POST .../contacts/sync` |
| `contactController.js:537, 997-1007` | `getClientSession` / `getChatsByLabelId` → API de labels |
| `contactController.js:815-816` | Tratamento de erro `pupPage` deixa de existir |
| `whatsappRoutes.js` | Reescrito como proxy fino (mesmo caminho `/api/v1/whatsapp/*` para o frontend) |
| `whatsappController.js` | `importLabels` via `GET .../labels` + `GET .../labels/:id/chats` |
| `authRoutes.js:119` | `stopSession` → `POST .../disconnect` |
| `businessRoutes.js:576` | `sendWWebJSMessage` → `POST .../messages` |
| `tagService.js:79, 155-171, 231, 267` | `getLabels`/`updateLabel`/`deleteLabel`/`createLabel` → API |
| `aiTools.js:264-297`, `campaignScheduler.js:329-370` | `sendImage` → `POST .../messages/media` |
| `services/message.js:119-150` | Gravar `timestamp` original (D8) |

### 15.6 Dados no MongoDB — a parte que engana

| Item | Hoje | Ação |
| :--- | :--- | :--- |
| `BusinessConfig.whatsappProvider` | enum `['twilio','wwebjs']`, default `wwebjs` (`BusinessConfig.js:27`) | enum `['twilio','squamata']` + `updateMany({ whatsappProvider: 'wwebjs' }, { $set: { whatsappProvider: 'squamata' } })` **na mesma janela do deploy** — senão o `enum` rejeita o valor antigo no próximo save |
| `campaignScheduler.js:349, 357, 363, 376, 400` | passa `'wwebjs'` literal em 5 pontos | `'squamata'` |
| `updates/barbeiro.json:4`, `updates/tatuador.json:28`, `updates/fixConfig.js:36` | seeds gravam `wwebjs` | `squamata` |
| `Contact` / `Message` / `Appointment` / `BusinessConfig.whatsappSessionId` | `ObjectId` (`ref: 'Session'`) | `String`; **zerar** os valores antigos e preencher no primeiro `connect` (não há `populate` nem número em produção — não vale escrever conversor) |
| `Message.waMessageId` (índice único esparso) | — | intacto; é o que garante dedup no reenvio |

### 15.7 Testes

| Situação | Ação |
| :--- | :--- |
| **10 arquivos** de teste referenciam/mockam `services/wwebjsService.js`: `crm`, `agent_chat`, `import`, `automation`, `tag_protection`, `unit/messageFilter`, `unit/messageHandler_lazy`, `unit/sync_verification`, `integration/business_config_verification`, `integration/membership_offboarding` | passar a mockar `services/whatsapp/client.js` com a **mesma superfície** (`sendUnifiedMessage`, `presence`, `sendMedia`) |
| `tests/mocks.js` exporta `getSessionStatus`/`getSessionQR` do módulo antigo | reescrever para o adaptador |
| `tests/integration/flow.test.js:79` e `tests/automation.test.js:43,104` | string `'wwebjs'` → `'squamata'` |
| `tests/unit/messageHandler_lazy.test.js` | usa `provider: 'whatsapp'` (terceiro nome) → alinhar em `'squamata'` |

### 15.8 Ordem de execução

| Etapa | Quando | Conteúdo | Critério |
| :--- | :--- | :--- | :--- |
| **L0** | já (risco zero) | Remover `qrcode`, `qrcode-terminal` e `tests/mocks/whatsappMock.js` — os três já estão mortos | `npm ls qrcode qrcode-terminal` vazio; suíte verde |
| **L1** | F3/F4 | Criar `services/whatsapp/{client,inbound}.js`; nenhum arquivo antigo é tocado | Novo caminho funciona em paralelo ao antigo no dev |
| **L2** | F4 | Trocar os consumidores (15.5) pelo adaptador | Fluxo completo: receber → IA → responder → campanha |
| **L3** | F8 | Deletar arquivos (15.1), dependências (15.2), Docker (15.3), Git (15.4) | Greps de 15.9 vazios |
| **L4** | F8 | Migração de dados (15.6) na mesma janela do deploy | `whatsappProvider` 100% `squamata`; sessões criadas no novo serviço |
| **L5** | F8 | Docs: `AI_CONTEXT.md:12`, `ARCHITECTURE.md:32,44,47`, badge `readme.md:4` | Nenhuma menção a `wwebjs`/Puppeteer |
| **L6** | F8 | Aceite final | Greps + smoke test + teste de reconexão |

### 15.9 Critérios de aceite (comandos)

```bash
# 1. Nada de biblioteca morta no código
grep -rn "wwebjs\|whatsapp-web\|puppeteer\|pupPage\|RemoteAuth\|UnifiedMongoStore" backend/ --include=*.js
# esperado: nenhum resultado (fora de node_modules)

# 2. Nada no manifesto, no .gitignore nem no .dockerignore
grep -rn "wwebjs\|puppeteer" backend/package.json .gitignore backend/.dockerignore

# 3. Nada instalado
npm ls whatsapp-web.js puppeteer qrcode qrcode-terminal
# esperado: "empty" / erro de pacote não encontrado
```

Complemento não automatizável: imagem do backend sem Chromium, smoke test verde (skill `calango-api-smoke-test`) e um ciclo real receber → responder.

### 15.10 O que **não** limpar

- **Twilio** (`twilioService.js`, `adaptTwilioMessage`, valor `'twilio'` no enum) — provedor alternativo declarado, não é resíduo.
- Chat público web (`publicChatRoutes.js`), `messageFilterService`, `menuService`, buffer de 11 s, handoff, `mediaProcessorService`, `visionService`, `transcriptionService` — nada disso é WhatsApp.
- Documentos de roadmap — são histórico do projeto; no máximo marcar como concluídos.

### 15.11 Risco da limpeza

| Risco | Mitigação |
| :--- | :--- |
| Deletar antes da virada → produto sem WhatsApp e sem rollback | Tag `pre-whatsapp-go` no git antes da L3; L3 só depois de F4 verde |
| `enum` de `whatsappProvider` rejeitar valor antigo | Migração de dados na mesma janela do deploy (15.6) |
| Teste continuar verde por mock que não existe mais | L3 inclui trocar os mocks; suíte tem de rodar contra o adaptador novo |

---

### 15.12 Documentação e specs (higiene)

A limpeza não é só de código. Hoje o ecossistema acumula documento por etapa: `PLANEJAMENTO.md` (593 linhas), `MEMORY.md`, `README.md` e, no Calango-bot, **6 roadmaps** — incluindo o `Roadmap 2 ... .LEGADO.md`. Spec demais é *overengineering*: a mesma informação em três lugares que divergem.

**Um spec por iniciativa, não por etapa.** As fases F0–F8 vivem **dentro** deste arquivo. Criar `f2-pareamento.md`, `f3-entrada.md`… seria exatamente o padrão que se quer evitar.

**Ciclo de vida — para onde o conteúdo vai ao concluir:**

| Conteúdo | Destino permanente |
| :--- | :--- |
| Decisão com trade-off | **ADR** (`Calango-docs/docs/ADRs`) — já é o caso do ADR 005 |
| O que ainda falta | Roadmap do repositório |
| O que foi feito e as armadilhas | `MEMORY.md` — **uma entrada consolidada do plano inteiro** (§16 do arquivo de regras) |
| Como usar | `README.md` |
| Execução em andamento | Spec/planejamento (temporário) |

Fonte de verdade em caso de divergência: **ADR > roadmap > MEMORY > spec > README**.

**Inventário atual e destino de cada documento:**

| Repositório | Documento | Ação |
| :--- | :--- | :--- |
| `Calango-docs` | 5 ADRs | **Manter** — é o registro permanente |
| `Calango-docs` | `docs/spec/ai-rules.md` | **Manter** — é norma, não spec |
| `Calango-docs` | `docs/templates/spec-template.md` | **Manter** — modelo |
| `Squamata-whatsapp` | `PLANEJAMENTO.md` | Ao fim da F8: decisões → ADRs, pendências → roadmap, feito/armadilhas → `MEMORY.md`, **apagar** |
| `Squamata-whatsapp` | `MEMORY.md`, `README.md`, `Dockerfile`, etc. | **Manter** |
| `Calango-bot` | `Roadmap 1` (concluído) e `Roadmap 3–5` | Consolidar o que foi feito em `docs/MEMORY.md`; **apagar** os concluídos |
| `Calango-bot` | `Roadmap 2 ... .LEGADO.md` | **Apagar** — superseded pelo Roadmap 2 atual; o histórico fica no git |
| `Calango-bot` | `Roadmap 2 - Múltiplos WhatsApps.md` | **Manter** durante a migração; ao fim, sobra só o que virou pendência futura (F5–F8) |

**Limite:** no máximo **um spec ativo por iniciativa** e nenhum spec ativo sem execução em andamento. Documento concluído é deletado — `git log` é o arquivo morto.

> Regra gêmea, no `Calango-docs`: a memória recebe **um resumo consolidado ao final do plano**, não um bloco por etapa. Detalhe de cada etapa pertence ao PR.

---

## 16. Publicação no GitHub (depois do plano aprovado)

1. `git init` + `.gitignore` já cobre `/data/`, `*.db`, `.env`, `backup-*.db`.
2. `.dockerignore` (o `constitution.md` exige `.env` fora da imagem).
3. `README.md` curto: o que é, como rodar em dev (`go run`), como rodar em produção (compose), links para ADR 002/005.
4. `LICENSE` (a definir) e `CODEOWNERS` se fizer sentido.
5. Primeiro commit: esqueleto + este plano. **Sem** `.env`, sem `data/`, sem binário.
6. Criar o repositório remoto e `git push -u origin main`.
