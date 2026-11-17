# Changelog

## 0.2.0 (2026-11-18)

- New `sendio transactional` commands for transactional emails (a saved send of a published template, sent by id): `create`, `list` (alias `ls`), `get`, `update`, `delete` (aliases `rm`, `remove`), `test` and `stats`. Tracking is off by default; `--track-opens` and `--track-clicks` turn it on.
- `sendio emails send --transactional tx_... --var CODE=123456 --to ada@example.com` sends a transactional email. The transactional email sets the sender, subject and content, so `--from`, `--subject`, `--reply-to`, `--html`, `--text` and `--template` are refused with it.
- Deprecated: `sendio emails send --template` (sending a template by template id). Use a transactional email instead; `--template` still works for now.
- New `sendio templates publish-impact` shows what a publish changes for a template's transactional emails.
- New `sendio broadcasts pause`, `resume` and `continue`.
- `sendio domains` shows each DNS record's `host` (what to type into the DNS provider, relative to the DNS zone) and prints the DNS zone.
- The free inbox flags more risky attachment types (for example `.html`, `.svg`, `.sh`, `.dmg`), the same list as the web console.

## 0.1.0 (2026-11-10)

First public release.
