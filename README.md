# Squamata-whatsapp

Conexão de WhatsApp do ecossistema Calango/Squamata como microsserviço independente, em Go, usando [`whatsmeow`](https://github.com/tulir/whatsmeow).

Substitui o `whatsapp-web.js` + Puppeteer que hoje roda **dentro** do processo Node do Calango-bot: sem Chromium, sem scraping de página, uma sessão por número em processo próprio.

## Status

| Etapa | Situação |
| :--- | :--- |
| Planejamento (paridade, contrato, 12 decisões) | ✅ [PLANEJAMENTO.md](./PLANEJAMENTO.md) |
| Spike do store (`sqlstore` + `modernc.org/sqlite`) | ✅ `internal/store` |
| **F1 — esqueleto HTTP, tabela de sessões, auth interna, CRUD** | ✅ |
| F2 — pareamento (QR gerado no Go, máquina de estados, reconexão) | ⏳ pendente |

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

## API

Todas as rotas, exceto `health`, exigem `Authorization: Bearer <SQUAMATA_INTERNAL_TOKEN>`.

| Método | Rota | O que faz |
| :--- | :--- | :--- |
| `GET` | `/api/v1/health` | Processo **e** banco (store acessível + contagem de sessões) |
| `POST` | `/api/v1/tenants/{tenantId}/sessions` | Cria sessão (`{"name":"Vendas","ownerUserId":"opcional"}`) |
| `GET` | `/api/v1/tenants/{tenantId}/sessions` | Lista sessões do tenant |
| `GET` | `/api/v1/tenants/{tenantId}/sessions/{id}` | Detalhe |
| `DELETE` | `/api/v1/tenants/{tenantId}/sessions/{id}` | Remove (a partir da F2: logout + remoção do device) |

```bash
curl -s localhost:3006/api/v1/health
curl -s -X POST localhost:3006/api/v1/tenants/t1/sessions \
  -H "Authorization: Bearer dev" -H "Content-Type: application/json" \
  -d '{"name":"Vendas"}'
```

O `tenantId` está no **caminho** de propósito: um `sessionId` de outro tenant não resolve, então o isolamento não depende de lembrar de filtrar.

## Testes

```bash
go test ./...
```

Cobrem: migração idempotente, CRUD, isolamento por tenant, persistência após reabrir o banco, auth (401), health saudável e degradado (503) e validação de entrada (400).

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
