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
  index.json                              # JID-keyed snapshot for incremental tools
  120363012345678901@g.us/                # group JID is the stable directory name
    2025-01/
      messages.jsonl                      # one JSON object per line
      media/
        a3f8b2c1d4e5f6a7.jpg
        b1c2d3e4f5a6b7c8.mp4
```

The directory name is the WhatsApp group JID rather than the group name,
so renames on the phone never split a group's history. The human name
lives in each JSONL line (`group_name`) and in `index.json` (`label`).

Each line in `messages.jsonl` contains the message ID, timestamp, sender,
type, text, and (for media) the relative path and hash. Optional fields are
omitted when empty. See [`spec.md`](spec.md) for the full schema.

### Incremental processing via `index.json`

`backup/index.json` is rewritten every 30 s (and on shutdown) with the
latest ingestion progress per group:

```json
{
  "schema": 1,
  "updated_at": "2026-05-25T13:00:00Z",
  "groups": {
    "120363428945290436@g.us": {
      "label": "Erik <> Aylan",
      "slug": "erik--aylan",
      "first_message_ts": "2026-03-12T08:00:00Z",
      "last_message_ts": "2026-05-25T11:27:36Z",
      "last_message_id": "3EB05A9FEBC9878F64FEA4",
      "message_count": 93,
      "latest_month": "2026-05"
    }
  }
}
```

Downstream summarizers / agents can read this single file and decide
which groups have new activity since their last run without parsing
every `messages.jsonl`. Writes are atomic (`tmp` + `rename`), so the
file is never observed in a partial state.

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
go run ./cmd --setup     # interactive wizard
go run ./cmd             # start the daemon
```

`--setup` walks you through choosing paths, pairing via QR, and picking
which groups to monitor with a checkbox-style picker. It writes a `.env`
in the project root which the daemon reads on startup. Re-run any time to
adjust — previous values come back as defaults and the old `.env` is
saved to `.env.bak.<timestamp>` before being overwritten.

If you'd rather skip the wizard, copy `.env.example` to `.env` and edit
by hand. The daemon also accepts plain environment variables.

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

Group JIDs look like `123456789-1234567890@g.us`. Once you've paired the
daemon once (so `wa-session.db` exists), use the built-in subcommand:

```bash
go run ./cmd --list-groups                      # top 10 by most-recent message
go run ./cmd --list-groups --all                # everything
go run ./cmd --list-groups --search familia     # case-insensitive name filter
go run ./cmd --list-groups --sort members       # alternative sorts: recent|name|members|created
go run ./cmd --list-groups --json | jq .        # machine-readable
go run ./cmd --list-groups --no-header | cut -f1   # JIDs only (header is also auto-stripped when piping)
```

"Most recent" is derived from the existing backup (last line of the latest
monthly JSONL per group). Groups you haven't backed up yet fall to the
bottom, ordered by group creation date.

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
