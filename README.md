# Squamata-whatsapp

Conexão de WhatsApp do ecossistema Calango/Squamata como microsserviço independente, em Go, usando [`whatsmeow`](https://github.com/tulir/whatsmeow).

Substitui o `whatsapp-web.js` + Puppeteer que hoje roda **dentro** do processo Node do Calango-bot: sem Chromium, sem scraping de página, uma sessão por número em processo próprio.

## Status

| Etapa | Situação |
| :--- | :--- |
| Planejamento (paridade, contrato, 12 decisões) | ✅ [PLANEJAMENTO.md](./PLANEJAMENTO.md) |
| Spike do store (`sqlstore` + `modernc.org/sqlite`) | ✅ `internal/store` |
| F1 — esqueleto HTTP, tabela de sessões, auth interna, CRUD | ✅ |
| **F2 — pareamento: QR gerado no Go, máquina de estados, eventos** | ✅ |
| F3 — entrada de mensagens (webhook + mídia + idempotência) | ⏳ pendente |

## Desenvolvimento

Docker **não** é usado em desenvolvimento — só no servidor de produção.

```bash
export SQUAMATA_INTERNAL_TOKEN=dev          # PowerShell: $env:SQUAMATA_INTERNAL_TOKEN = "dev"
go run ./cmd/squamata-whatsapp
```

Requisitos: Go ≥ 1.26 (exigido pela revisão atual do `whatsmeow`) e nada mais — o driver SQLite é Go puro, então não há CGO nem compilador C em nenhum ambiente.

Variáveis:

| Variável | Padrão | Observação |
| :--- | :--- | :--- |
| `SQUAMATA_INTERNAL_TOKEN` | — | **Obrigatória.** Sem ela o serviço não sobe (fail-fast) |
| `SQUAMATA_WHATSAPP_PORT` | `3006` | |
| `SQUAMATA_WHATSAPP_DB_PATH` | `./data/whatsapp.db` | Em produção: `/data/whatsapp.db` (volume) |
| `SQUAMATA_WEBHOOK_URL` | — | Webhook do Calango-bot. Vazio = eventos desligados |
| `SQUAMATA_WEBHOOK_SECRET` | — | Segredo do HMAC em `X-Signature` |

## API

Todas as rotas, exceto `health`, exigem `Authorization: Bearer <SQUAMATA_INTERNAL_TOKEN>`.

| Método | Rota | O que faz |
| :--- | :--- | :--- |
| `GET` | `/api/v1/health` | Processo **e** banco (store acessível + contagem de sessões) |
| `POST` | `/api/v1/tenants/{tenantId}/sessions` | Cria sessão (`{"name":"Vendas","ownerUserId":"opcional"}`) |
| `GET` | `/api/v1/tenants/{tenantId}/sessions` | Lista sessões do tenant |
| `GET` | `/api/v1/tenants/{tenantId}/sessions/{id}` | Detalhe |
| `POST` | `/api/v1/tenants/{tenantId}/sessions/{id}/connect` | Inicia o client; idempotente |
| `POST` | `/api/v1/tenants/{tenantId}/sessions/{id}/disconnect` | Para o client e **mantém** o pareamento |
| `GET` | `/api/v1/tenants/{tenantId}/sessions/{id}/qr` | `{ qr, image, expiresAt }` — `409` quando não há QR válido |
| `DELETE` | `/api/v1/tenants/{tenantId}/sessions/{id}` | Logout + remoção do device + registro |

```bash
curl -s localhost:3006/api/v1/health
curl -s -X POST localhost:3006/api/v1/tenants/t1/sessions \
  -H "Authorization: Bearer dev" -H "Content-Type: application/json" \
  -d '{"name":"Vendas"}'
```

O `tenantId` está no **caminho** de propósito: um `sessionId` de outro tenant não resolve, então o isolamento não depende de lembrar de filtrar.

## Eventos (webhook)

Com `SQUAMATA_WEBHOOK_URL` definido, o serviço publica eventos assinados para o Calango-bot:

| Evento | Quando |
| :--- | :--- |
| `session.status` | Toda transição: `initializing`, `qrcode`, `authenticated`, `ready`, `disconnecting`, `disconnected`, `error` |
| `session.qr` | A cada rotação do QR (~20 s), com `qr` (payload) e `image` (PNG em data URI) |
| `session.auth_failure` | Conexão encerrada pelo celular ou substituída por outro aparelho |

- Assinatura: `X-Signature: sha256=<hex>`, HMAC-SHA256 do **corpo bruto** com `SQUAMATA_WEBHOOK_SECRET`.
- Entrega best-effort com 3 tentativas (1 s, 2 s). Mensagens (F3) terão outbox persistido.
- `HistorySync` **nunca** é publicado: o histórico do produto vive no Mongo (decisão D5).

## Ciclo de pareamento

```
disconnected ──connect──▶ initializing ──QR──▶ qrcode ──leitura──▶ authenticated ──▶ ready
      ▲                        │                                     │
      │                        └── 120 s sem leitura → disconnected  │
      └──────── disconnect ─────────────────────────────────────────┘
                     queda → initializing → ready  (reconexão automática)
```

- `disconnect` **mantém** o device: reconectar não pede QR de novo.
- `DELETE` é o único caminho que descarta o device (com logout no celular).
- Celular desconectou o aparelho (`LoggedOut`) ou outra sessão assumiu (`StreamReplaced`): status `error` + `session.auth_failure`, device preservado até um `DELETE` explícito (decisão D6).
- No boot, sessões com `device_jid` reconectam sozinhas (até 4 tentativas com backoff).

## Testes

```bash
go test ./...
```

Cobrem: migração v1→v2 e idempotência, CRUD, isolamento por tenant, persistência após reabrir o banco, auth (401), health saudável e degradado (503), validação de entrada (400), PNG do QR e assinatura HMAC do webhook.

## Produção (Docker)

```bash
cp .env.example .env        # definir SQUAMATA_INTERNAL_TOKEN
docker compose build
docker compose up -d
```

- Imagem multi-stage: `golang:1.26-alpine` → `distroless/static` (sem shell, sem CGO). **Go não é necessário no servidor** — o compilador vive no estágio de build.
- O binário tem a flag `-health`, usada no `HEALTHCHECK` (distroless não tem `curl`).
- Estado em volume nomeado `whatsapp-data:/data`; a rede `squamata-global` é externa e precisa existir.

## Estrutura

```
cmd/squamata-whatsapp/   binário + flag -health
internal/config/         variáveis de ambiente, fail-fast
internal/httpapi/        rotas, auth interna, health, CRUD
internal/sessions/       tabela `sessions` (metadados) e repositório
internal/store/          sqlstore (estado criptográfico) no mesmo arquivo SQLite
```

## Documentação

- [PLANEJAMENTO.md](./PLANEJAMENTO.md) — matriz de paridade com o `wwebjsService`, contrato HTTP, envelope de eventos, fases F0–F8, decisões fechadas e plano de limpeza do Calango-bot.
- **ADR 005** (repo `Calango-docs`) — por que SQLite/`sqlstore` e não MongoDB, Postgres ou store próprio.
- **ADR 002** (repo `Calango-docs`) — versionamento de API e healthcheck.
- **Roadmap 2** (repo `Calango-bot`) — migração dos múltiplos WhatsApps.
