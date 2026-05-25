# wa-backup

A Go daemon that backs up WhatsApp group messages and media to your local
filesystem. Connects via [`go.mau.fi/whatsmeow`](https://github.com/tulir/whatsmeow)
(no browser, native multidevice protocol), captures the initial history sync
from the phone, then keeps running to archive new messages as they arrive.

> **Heads-up:** unofficial WhatsApp clients violate Meta's Terms of Service.
> Running this against your own account is at your own risk and can lead to
> a temporary or permanent ban. Use it only on accounts you own and only for
> personal backup of conversations you legitimately have access to.

## What it saves

For each monitored group, a directory tree like:

```
backup/
  familia-doe/
    2025-01/
      messages.jsonl        # one JSON object per line
      media/
        a3f8b2c1d4e5f6a7.jpg
        b1c2d3e4f5a6b7c8.mp4
```

Each line in `messages.jsonl` contains the message ID, timestamp, sender,
type, text, and (for media) the relative path and hash. Optional fields are
omitted when empty. See [`spec.md`](spec.md) for the full schema.

Supported message types: text, image, video, audio/voice note, document,
sticker, poll, reaction, location. Anything else is stored with
`"type": "unknown"` and a raw JSON dump of the protobuf so nothing is lost.

## Requirements

- Go 1.24+ (built and tested on 1.25)
- A WhatsApp account with multidevice enabled
- A machine that can run continuously (a Mac Mini, a small server, a Pi…)

No CGo. SQLite is via [`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite),
which is pure Go.

## Quick start

```bash
git clone https://github.com/AylanBoscarino/wa-backup.git
cd wa-backup
cp .env.example .env
# edit .env if you want non-default paths / filters
go run ./cmd
```

On first run the binary prints a QR code on stderr. Open WhatsApp on your
phone → **Settings → Linked devices → Link a device**, and scan it. The
session is saved to `wa-session.db`; subsequent runs reconnect silently.

## Configuration

All knobs are environment variables. They can also live in a `.env` file in
the project root — actual environment variables override the file.

| Variable | Default | Description |
|---|---|---|
| `LOCAL_BACKUP_PATH` | `./backup` | Filesystem root for the JSONL + media tree |
| `SESSION_DB_PATH` | `./wa-session.db` | SQLite file holding the WhatsApp session |
| `GROUP_ALLOWLIST` | *(empty)* | Comma-separated group JIDs to monitor |
| `GROUP_DENYLIST` | *(empty)* | Comma-separated group JIDs to ignore (only used when allowlist is empty) |
| `MEDIA_WORKERS` | `4` | Parallel media download goroutines |
| `LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `HISTORY_SYNC_IDLE` | `30s` | Idle timeout to declare the history-sync burst complete |

### Finding group JIDs

Group JIDs look like `123456789-1234567890@g.us`. The simplest way to
discover yours today is to run the daemon once with both lists empty (it
will save every group), then read the `group_jid` field in the resulting
JSONL files or set `LOG_LEVEL=debug` to see them in the logs. A
`--list-groups` subcommand is planned.

## Architecture

```
cmd/main.go                    entrypoint, signal handling, graceful shutdown
config/                        env + .env loading and validation
internal/client/               whatsmeow init, QR pairing, exponential backoff reconnect
internal/handler/              events.Message and events.HistorySync handlers
internal/pipeline/             type detection, JSONL writer, media worker pool
internal/storage/              Storage interface + LocalStorage with global hash dedup
```

The pipeline is identical for live messages and history-sync replays:
`HistorySync` batches are unwrapped into `(types.MessageInfo, *waE2E.Message)`
pairs and fed through the same `Processor.Process` call. Media downloads
happen in a worker pool with dedup keyed on `SHA-256(MediaKey)[:16]`.

## Reconnection

The daemon disables whatsmeow's built-in auto-reconnect and runs its own
loop so we can match the SPEC's policy:

- **Transient disconnect**: exponential backoff (1s → 2s → 4s → … capped at 5 min)
- **Temporary ban**: honour the WhatsApp-reported duration, falling back to ~1 h
- **LoggedOut / StreamReplaced**: fatal — the session is invalid and you need
  to delete `wa-session.db` and scan a new QR

## Graceful shutdown

`SIGINT` or `SIGTERM` triggers:

1. Stop accepting new events
2. Drain in-flight media downloads (bounded to 30 s)
3. Disconnect the WhatsApp client
4. Close JSONL writers
5. Log a summary (messages processed, media downloaded/deduped/failed)

## Security caveats

See [SECURITY.md](SECURITY.md). The short version:

- `wa-session.db` is equivalent to your WhatsApp credentials. Protect it.
- `backup/` contains unencrypted personal data of you and third parties.
  Consider full-disk encryption on the host, and think about the privacy /
  data-protection (LGPD/GDPR) implications before backing up groups whose
  members haven't been informed.

## Not implemented (roadmap)

- Cloud storage backends (S3 / GCS / R2) — the `Storage` interface is
  designed for them but only `LocalStorage` exists today
- Search / web UI
- Backup of the session DB to cloud
- Backup-at-rest encryption
- Individual chats (groups only for now)
- Export to HTML / ZIP

## License

[MIT](LICENSE).
