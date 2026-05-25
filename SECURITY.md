# Security policy

## Sensitive material this tool handles

- **`wa-session.db`** stores the cryptographic keys that link your WhatsApp
  account to the daemon. Anyone with read access to this file can impersonate
  your account on WhatsApp Web. Never commit it, never share it, never copy
  it to an untrusted machine. The `.gitignore` blocks `*.db`, but the
  protection is only as strong as your filesystem permissions.
- **`backup/`** contains plaintext copies of personal conversations and media
  belonging to you and third parties. Treat this directory with the same care
  you would a mailbox archive: it is not encrypted at rest.
- **`.env`** may hold paths, group identifiers, and other tuning knobs. It is
  gitignored — keep it that way.

## File permission policy

The daemon enforces owner-only permissions on everything it creates:

- Files (`messages.jsonl`, media, `wa-session.db` and its `-wal`/`-shm`/`-journal`
  sidecars) are created with mode **0600**.
- Directories (backup root, per-group, per-month, `media/`, session DB parent)
  are created with mode **0700**.
- `syscall.Umask(0o077)` is set as the very first line of `main()` so that
  any file the runtime or a dependency creates also defaults to owner-only.
- On startup the daemon walks the backup root once and tightens any older
  file or directory that is more permissive than the target mode. It never
  loosens permissions you may have hardened further by hand.

### Caveats

- **Non-POSIX filesystems** (FAT, exFAT, some network mounts) silently ignore
  POSIX modes. If you point `LOCAL_BACKUP_PATH` at one, `chmod` is a no-op
  and the files inherit the volume's default visibility. Prefer APFS / HFS+ /
  ext4 / ZFS for the backup directory.
- **Existing installs** that ran an earlier version still benefit from the
  startup tightening pass, but only for files inside `LOCAL_BACKUP_PATH`.
  Move legacy session DBs into a 0700 directory yourself.

## Reporting a vulnerability

If you find a security issue (credential leak path, file-permission flaw,
deserialization bug, etc.), please email the maintainer directly rather than
opening a public issue. Include enough detail to reproduce and an estimate of
impact. You can expect an acknowledgement within a few days.

## Out of scope

- WhatsApp account bans resulting from running an unofficial client — this is
  a Terms of Service issue, not a security vulnerability in this codebase.
- Loss of media that expired on WhatsApp's servers before the daemon could
  download it. Failures are logged with the message ID so you can identify
  what was lost.
- Bugs in upstream dependencies (`go.mau.fi/whatsmeow`, `modernc.org/sqlite`,
  etc.) — report those to the respective projects.
