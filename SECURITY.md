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
