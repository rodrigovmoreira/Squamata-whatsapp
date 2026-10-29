# Squamata-whatsapp

Conexão de WhatsApp do ecossistema Calango/Squamata como microsserviço independente, em Go, usando [`whatsmeow`](https://github.com/tulir/whatsmeow).

Substitui o `whatsapp-web.js` + Puppeteer que hoje roda **dentro** do processo Node do Calango-bot: sem Chromium, sem scraping de página, uma sessão por número em processo próprio.

## Status

| Etapa | Situação |
| :--- | :--- |
| Planejamento (paridade, contrato, 12 decisões) | ✅ [PLANEJAMENTO.md](./PLANEJAMENTO.md) |
| Spike do store (`sqlstore` + `modernc.org/sqlite`) | ✅ `internal/store` com teste |
| F1 — esqueleto HTTP, tabelas de sessão, auth interna | ⏳ pendente |

## Desenvolvimento

Docker **não** é usado em desenvolvimento — só no servidor de produção.

```bash
export SQUAMATA_INTERNAL_TOKEN=dev          # PowerShell: $env:SQUAMATA_INTERNAL_TOKEN = "dev"
go test ./...
go run ./cmd/squamata-whatsapp              # a partir da F1
```

Requisitos: Go ≥ 1.26 (exigido pela revisão atual do `whatsmeow`) e nada mais — o driver SQLite é Go puro, então não há CGO nem compilador C em nenhum ambiente.

## Estrutura

```
cmd/squamata-whatsapp/   binário
internal/config/         variáveis de ambiente, fail-fast
internal/store/          sqlstore (estado criptográfico) + metadados de sessão
```

## Documentação

- [PLANEJAMENTO.md](./PLANEJAMENTO.md) — matriz de paridade com o `wwebjsService`, contrato HTTP, envelope de eventos, fases F0–F8, decisões fechadas e plano de limpeza do Calango-bot.
- **ADR 005** (repo `Calango-docs`) — por que SQLite/`sqlstore` e não MongoDB, Postgres ou store próprio.
- **ADR 002** (repo `Calango-docs`) — versionamento de API e healthcheck.
- **Roadmap 2** (repo `Calango-bot`) — migração dos múltiplos WhatsApps.
