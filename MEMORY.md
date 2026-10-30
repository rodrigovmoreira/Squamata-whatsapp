# MEMORY — aprendizados, armadilhas e pendências

> Coisas que descobrimos **na prática** e que não são óbvias lendo o código.
> Complementa o [PLANEJAMENTO.md](./PLANEJAMENTO.md) (decisões) e o [README.md](./README.md) (uso).

---

## 1. O que o `whatsmeow` exige (verificado na fonte e em execução)

| Fato | Consequência |
| :--- | :--- |
| `container.NewDevice()` cria o device **só em memória**; nada é gravado até o pareamento (`sqlstore` devolve `ErrDeviceIDMustBeSet` enquanto não há JID) | É por isso que existe a coluna `device_jid` (migração v2). Sem ela não há reconexão no boot |
| `sqlstore.New` abre o banco usando o **rótulo de dialeto como nome de driver** | Com `modernc.org/sqlite` (driver `sqlite`) só funciona `NewWithDB(db, "sqlite3")` + `Upgrade` explícito |
| `Container.Upgrade` aborta com *"foreign keys are not enabled"* | O DSN **precisa** de `_pragma=foreign_keys(1)` |
| `events.QR.Codes[0]` vem como link (`https://wa.me/settings/l?...`) na revisão fixada | Não é mais a string `2@...`. O painel **não deve interpretar** o conteúdo, só renderizar |
| O QR rotaciona a cada ~20 s | O serviço guarda o último válido; a janela total de pareamento é 120 s |
| `EnableAutoReconnect` já vem `true` | Queda transitória é reconectada pela biblioteca; não duplicar backoff |
| `LoggedOut` e `StreamReplaced` são `PermanentDisconnect` | Não reconectam: marcamos `error` + `session.auth_failure` |
| `client.DownloadAny(ctx, msg)` desembrulha qualquer tipo de mídia (view-once, ephemeral) | Não precisa de switch por campo para baixar |
| `client.Store.LIDs.GetPNForLID(ctx, jid)` resolve `@lid` → telefone | `@lid` não é telefone; resolver quando a sessão usa *addressing mode* LID |
| Log `Error sending close to websocket` no `Disconnect` | Ruído da biblioteca em WARN, não é falha nossa |

## 2. Operação e ambiente

- **`SetMaxOpenConns(1)`** (ADR 005): toda escrita no SQLite serializa. Não gera deadlock porque nunca deixamos `rows` aberto e disparamos outra query na mesma função.
- **Imagem distroless não tem shell nem `curl`**: o `HEALTHCHECK` usa o próprio binário (`squamata-whatsapp -health`).
- **`PRAGMA user_version` pertence ao pacote `sessions`**. A tabela `outbox` usa `CREATE TABLE IF NOT EXISTS` para não disputar o mesmo contador de migração.
- **Testes no Windows**: `t.TempDir()` falha na limpeza ("A pasta não está vazia") quando o SQLite ainda segura o arquivo por alguns ms. Por isso existe o helper `tempDir` com retry em `internal/sessions/repo_test.go`.
- **Docker CLI existe nesta máquina, mas o daemon não está rodando** (`npipe:////./pipe/dockerDesktopLinuxEngine` ausente). Validação de imagem fica no servidor.
- **Pipeline de eventos verificado de ponta a ponta** (2026-10-29): outbox → drain → POST assinado. Um listener local recebeu `session.status` (initializing, qrcode) e `session.qr` (payload 277 chars, PNG 1998 chars) com `X-Signature` válido.
- **Todo evento passa pelo outbox**, inclusive os de sessão. O envio em memória só acontece quando não há fila (dev/testes) ou se o INSERT falhar.

## 2.1 Mensagens (F3, lado serviço)

- `events.Message` cobre **entrada e saída**: `Info.IsFromMe` decide entre `message` e `message.sent`. Não existe evento separado como no `whatsapp-web.js` (`message_create`).
- `client.DownloadAny` resolve mídia já desembrulhada (view-once/ephemeral) — não é preciso tratar cada variante.
- Só `image` e `audio` têm bytes baixados (D1); vídeo e documento vão como metadado para não inflar o base64.
- O horário original da mensagem (`Info.Timestamp`) é preservado no evento: o `Emit` só preenche `timestamp` quando ele vem zero. Sem isso, o reenvio do outbox jogaria a mensagem para o fim da conversa (D8).

## 2.2 Envio (F4, lado serviço)

- Não existe *builder* de mídia na biblioteca: o envio de imagem é `client.Upload` → montar `waE2E.ImageMessage` com `URL`, `DirectPath`, `MediaKey`, `FileEncSHA256`, `FileSHA256`, `FileLength` → `SendMessage`. Esquecer um desses campos faz o WhatsApp rejeitar em silêncio.
- Só `image/jpeg`, `image/png` e `image/webp` são aceitos (paridade com o `sendImage` antigo); o resto é `ErrUnsupportedMime`.
- A URL é baixada com `io.LimitReader(max+1)`: detecta excesso sem carregar um arquivo gigante na memória.
- `parseTarget` replica a regra antiga do `wwebjsService`: até 12 dígitos ganha DDI `55`; acima disso é tratado como internacional.
- **Bug pego no teste manual:** sessão que existe no banco mas não tem `client` em memória respondia `404 SESSION_NOT_FOUND` em vez de `409 SESSION_NOT_CONNECTED` — o bot desistiria em vez de avisar "conecte a sessão". Só o repositório distingue os dois casos; há regressão em `send_test.go`.

## 3. O que ainda **não** dá para validar

| Item | Por que falta | O que destrava |
| :--- | :--- | :--- |
| Pareamento real (ler o QR e chegar a `ready`) | exige um número de WhatsApp | **chip de teste dedicado** (nunca número de cliente) |
| Reconexão de sessão **pareada** após restart | idem | parear uma vez, reiniciar o processo, conferir `ready` |
| `session.auth_failure` em `LoggedOut` / `StreamReplaced` | idem | deslinkar o aparelho pelo celular |
| `message` / `message.sent` com mensagem de verdade | a classificação (texto/imagem/áudio/vídeo/documento) e o pipeline de eventos estão testados; falta mensagem real | chip pareado + `inbound.js` no bot |
| `docker build` e `docker compose up` | daemon ausente na máquina de dev | rodar no Fedora server |
| Fluxo ponta a ponta com o Calango-bot | o webhook do bot já existe e foi validado com eventos reais (HMAC cruzado), mas nenhuma mensagem de verdade passou por ele | chip pareado |
| Envio de texto, mídia e presença | rotas de saída entregues no PR #1 (lado serviço); o lado bot é o próximo PR | PR do bot |

## 4. Decisões que mordem depois

- **Após `LoggedOut`, o device é preservado** (D6). Chamar `connect` de novo não volta a funcionar: é preciso `DELETE` e criar sessão nova.
- **`HistorySync` é ignorado de propósito** (D5). Se um dia importarmos histórico, tem de ser idempotente por `waMessageId` e **nunca** disparar IA.
- **Pairing code (8 dígitos) está adiado** (D10) — o QR resolve a migração.
- **Reparear não apaga o histórico do CRM**: `DELETE` remove device e registro da sessão no SQLite, nunca `Message`/`Contact` do Mongo.

## 5. Onde está a verdade

- Este `MEMORY.md` e o `README.md` são **permanentes**.
- `PLANEJAMENTO.md` é um **spec temporário**: ao fim da F8, suas decisões viram ADRs em `calango-docs` e o arquivo é **apagado**. Não aponte para ele como referência durável.
- Ordem de precedência em caso de divergência: **ADR > roadmap > MEMORY > spec > README**.
- A memória é escrita **ao final do plano**, como resumo consolidado — não um bloco por fase (`ai-rules.md` §4, itens 3 e 5 em `calango-docs`).
