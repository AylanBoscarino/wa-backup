# wa-backup — spec

## Goal

Daemon Go que conecta ao WhatsApp via whatsmeow e salva automaticamente todas as
mensagens e mídias de grupos selecionados no filesystem local do Mac Mini.

---

## Contexto e decisões já tomadas

- **Runtime**: Go 1.24+, sem CGo exceto onde whatsmeow exigir (sqlite3)
- **Library**: `go.mau.fi/whatsmeow` — conecta ao WhatsApp Web via WebSocket,
  sem browser, protocolo multidevice nativo
- **Storage desta fase**: filesystem local apenas. Cloud (S3/GCS) é roadmap futuro;
  não implementar agora, mas o código deve deixar a porta aberta via interface
- **Sessão**: SQLite local gerenciado pelo `sqlstore` do whatsmeow. Persiste entre
  restarts — sem SQLite não há sessão e o usuário precisa escanear QR de novo
- **Deploy**: processo contínuo no Mac Mini, sem container por enquanto

---

## Dois modos de operação

### 1. History sync (passivo, automático na 1ª conexão)

Quando o whatsmeow conecta em um dispositivo novo, o WhatsApp empurra
automaticamente o evento `events.HistorySync` com lotes de mensagens históricas
(até ~3 meses dependendo do telefone). O daemon deve:

- Capturar todos os lotes de `events.HistorySync`
- Processar cada mensagem pelo mesmo pipeline do modo reativo
- Detectar fim do sync (evento `events.HistorySyncComplete` ou idle de 30s sem
  novos lotes) e logar quantas mensagens/mídias foram salvas

### 2. Modo reativo (contínuo, para sempre)

Após o sync inicial, o daemon escuta `events.Message` indefinidamente e processa
cada mensagem nova pelo mesmo pipeline. Este é o modo principal de operação.

**O pipeline de processamento é idêntico nos dois modos** — history sync e modo
reativo entregam a mesma estrutura para o mesmo processador.

---

## Filtragem de grupos

O usuário configura via env vars quais grupos monitorar:

- `GROUP_ALLOWLIST` (recomendado): lista de JIDs separados por vírgula.
  Só salva mensagens desses grupos. Se vazio, usa denylist.
- `GROUP_DENYLIST`: lista de JIDs a ignorar. Só funciona se allowlist estiver vazia.
- Se ambos vazios: salva todos os grupos (cuidado com volume)

JID de grupo no WhatsApp tem formato: `123456789-1234567890@g.us`

O daemon deve logar na inicialização quais grupos está monitorando.

---

## Tipos de mensagem a processar

| Tipo | Campo protobuf | O que salvar |
|---|---|---|
| Texto | `GetConversation()` / `GetExtendedTextMessage()` | Texto completo |
| Imagem | `GetImageMessage()` | Download + thumbnail hash |
| Vídeo | `GetVideoMessage()` | Download |
| Áudio / voice note | `GetAudioMessage()` | Download |
| Documento / PDF | `GetDocumentMessage()` | Download + filename original |
| Sticker | `GetStickerMessage()` | Download |
| Enquete | `GetPollCreationMessage()` | Pergunta + opções como JSON |
| Reação | `GetReactionMessage()` | Emoji + MessageID alvo |
| Localização | `GetLocationMessage()` | Lat/lon + nome do lugar |

Tipos não listados (ex: contato vCard, chamada, sistema) devem ser salvos com
`type: "unknown"` e o raw JSON do protobuf para não perder dados.

---

## Formato de armazenamento

### Estrutura de diretórios

```
$BACKUP_PATH/                          # configurável via LOCAL_BACKUP_PATH
  {nome-do-grupo}/                     # nome sanitizado (sem chars especiais)
    {YYYY-MM}/
      messages.jsonl                   # uma linha JSON por mensagem
      media/
        {sha256[:16]}.{ext}            # mídia deduplicada por hash
```

Sanitização do nome de grupo: lowercase, espaços → hífens, remover tudo que não
for `[a-z0-9-]`, truncar em 64 chars.

### Formato de cada linha do JSONL

```json
{
  "id":        "ABCDEF123456",
  "timestamp": "2025-01-15T14:32:00Z",
  "group_jid": "123456789-1234567890@g.us",
  "group_name": "Família Doe",
  "sender_jid": "5511999999999@s.whatsapp.net",
  "sender_name": "João",
  "type":      "image",
  "text":      "olha isso",
  "media": {
    "hash":     "a3f8b2c1d4e5f6a7",
    "ext":      "jpg",
    "mime":     "image/jpeg",
    "size":     204800,
    "path":     "Família Doe/2025-01/media/a3f8b2c1d4e5f6a7.jpg"
  },
  "poll": null,
  "reaction": null,
  "reply_to": "PREVMSGID123",
  "raw": null
}
```

Campos nulos devem ser omitidos (`omitempty`). O campo `raw` só é preenchido para
tipos unknown.

### Deduplicação de mídia

Antes de baixar, calcular o hash SHA-256 do `MediaKey` (disponível antes do
download, é a chave de criptografia). Se o arquivo já existe no diretório de
media, pular o download. Isso evita baixar a mesma imagem enviada em múltiplos
grupos ou reenviada.

> Nota: o hash do MediaKey não é o mesmo que o hash do arquivo descriptografado.
> Se preferir consistência, fazer o hash do conteúdo após o download e renomear.
> Documentar a escolha no código.

---

## Pipeline interno

```
Event (HistorySync | Message)
  └─► Filtro de grupo
        └─► Detector de tipo
              ├─► TextWriter  → AppendJSONL(group, date, msg)
              └─► MediaJob    → channel → WorkerPool(N workers)
                                  └─► DownloadAny → hash → WriteFile → atualiza JSONL
```

### Worker pool de mídia

- `MEDIA_WORKERS` goroutines (default: 4) consumindo um canal de `MediaJob`
- Cada job contém: a mensagem, o tipo de mídia, o path de destino
- Se o download falhar, logar o erro com o MessageID e continuar (não crashar)
- Mídia expira nos servidores do WhatsApp em horas/dias — falhas de download
  devem ser logadas de forma que o usuário possa identificar o que foi perdido

---

## Reconexão automática

O whatsmeow emite `events.Disconnected` com um motivo. O daemon deve:

- **Reconectar com backoff exponencial** (1s → 2s → 4s → ... → max 5min) para:
  `DisconnectReasonConnectionLost`, `DisconnectReasonConnectionClosed`,
  `DisconnectReasonTemporaryBan` (aguardar mais, ~1h)
- **Não reconectar e encerrar com erro** para:
  `DisconnectReasonLoggedOut`, `DisconnectReasonTakenOver`
  (sessão inválida — usuário precisa escanear QR de novo)
- Logar o motivo da desconexão sempre

---

## Configuração (env vars)

| Variável | Default | Descrição |
|---|---|---|
| `LOCAL_BACKUP_PATH` | `./backup` | Raiz do backup no filesystem |
| `SESSION_DB_PATH` | `./wa-session.db` | Path do SQLite de sessão |
| `GROUP_ALLOWLIST` | `""` | JIDs separados por vírgula |
| `GROUP_DENYLIST` | `""` | JIDs separados por vírgula |
| `MEDIA_WORKERS` | `4` | Goroutines de download |
| `LOG_LEVEL` | `info` | debug / info / warn / error |
| `HISTORY_SYNC_IDLE` | `30s` | Timeout de idle para detectar fim do sync |

Suporte a `.env` file na raiz do projeto (carregar antes das env vars do sistema,
env vars do sistema têm prioridade).

---

## Estrutura de pacotes

```
wa-backup/
├── cmd/
│   └── main.go              # entrypoint, lifecycle, signal handling
├── internal/
│   ├── client/
│   │   └── client.go        # init whatsmeow, QR via terminal, reconexão
│   ├── handler/
│   │   ├── history.go       # captura events.HistorySync
│   │   └── message.go       # captura events.Message
│   ├── pipeline/
│   │   ├── processor.go     # detecta tipo, roteia para writer ou media job
│   │   ├── writer.go        # AppendJSONL, serialização
│   │   └── downloader.go    # worker pool, DownloadAny, hash, WriteFile
│   └── storage/
│       ├── storage.go       # interface Storage (preparado para cloud futuro)
│       └── local.go         # LocalStorage — implementação filesystem
├── config/
│   └── config.go            # Load() lê env vars + .env, valida, retorna Config
├── .env.example             # template de configuração
├── .gitignore               # ignorar backup/, *.db, .env
└── SPEC.md                  # este arquivo
```

---

## Interface Storage (preparar para o futuro sem implementar)

```go
// storage/storage.go
type Storage interface {
    // AppendMessage adiciona uma linha ao JSONL do grupo/mês.
    AppendMessage(groupSlug, yearMonth string, line []byte) error

    // WriteMedia salva o conteúdo de mídia e retorna o path relativo.
    // Se já existir (dedup por hash), retorna o path sem reescrever.
    WriteMedia(hash, ext string, data []byte) (relativePath string, err error)

    // Exists verifica se uma mídia já foi salva (dedup).
    Exists(hash, ext string) (bool, error)

    // Close libera recursos (flush de buffers, fecha arquivos).
    Close() error
}
```

A implementação `LocalStorage` é a única necessária agora. A interface existe para
que no futuro `S3Storage` / `GCSStorage` sejam drop-in replacements.

---

## Graceful shutdown

`main.go` deve capturar `SIGINT` e `SIGTERM` e:

1. Parar de aceitar novos eventos
2. Aguardar o worker pool de mídia terminar os downloads em andamento (timeout: 30s)
3. Fechar o client whatsmeow (`client.Disconnect()`)
4. Fechar o storage (`storage.Close()`)
5. Logar resumo: mensagens processadas, mídias baixadas, erros

---

## Logging

Usar `go.uber.org/zap` em modo estruturado (JSON em prod, console colorido em dev).
`LOG_LEVEL=debug` deve mostrar cada mensagem processada. `LOG_LEVEL=info` só mostra
eventos relevantes (conexão, grupos, erros, resumo de sync).

---

## Não implementar nesta fase

- Interface web ou CLI de busca
- Cloud storage (S3/GCS/R2) — interface preparada, implementação no roadmap
- Serialização do SQLite para o bucket — roadmap
- Criptografia do backup local
- Suporte a chats individuais (só grupos por enquanto)
- Export para outros formatos (HTML, ZIP)

---

## Como usar o projeto com Claude Code

```bash
# Na raiz do projeto, com esta spec:
claude --goal "Implemente o wa-backup conforme SPEC.md. Comece por config/, depois storage/, client/, pipeline/, handler/ e por fim cmd/main.go. Não implemente nada que esteja na seção 'Não implementar nesta fase'."
```

---

## Checklist de aceitação

- [ ] `go build ./...` sem erros
- [ ] `go vet ./...` sem warnings
- [ ] Na primeira execução, exibe QR code no terminal e conecta ao escanear
- [ ] Na segunda execução, reconecta sem pedir QR (sessão persistida)
- [ ] Mensagens de texto de grupos da allowlist aparecem no JSONL correto
- [ ] Imagem enviada em grupo gera entrada JSONL + arquivo em `media/`
- [ ] Mesma imagem enviada duas vezes não duplica o arquivo (dedup)
- [ ] SIGINT encerra o processo limpo (sem panic, sem goroutine leak)
- [ ] `LOG_LEVEL=debug` mostra o tipo de cada mensagem processada
