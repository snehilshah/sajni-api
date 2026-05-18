# Sajni

## Overview

Sajni = PKMS at ohmysajni.com. AI = **Sajni** (Gemini backend). Two repos, split git history:

| Repo | Stack | Deploy |
| --- | --- | --- |
| `sajni-api/` | Go, Postgres, GCS | Cloud Run (`asia-south1`), tag `sga/release/v*` |
| `sajni-web/` | React 19, Vite, Tailwind v4, shadcn | Vercel, tag `srf/release/v*` |
| `sajni-design/` | HTML/JSX (claude.ai/design) | Static ref only |

See `DESIGN.md` if working on UI.

## AI agent (`internal/ai/`)

* Model: `gemini-2.5-flash` (override `GEMINI_MODEL`). Ban `-lite` -> silent stop post-tool.
* Modes: `palette` (max 6 tool rounds, short res) / `chat` (max 8 rounds, long res).
* Tools split: read (`list_tasks`, etc) & write (`create_task`, etc). Write = `Mutating: true`.
* Write tool return `meta` `{kind, id, title, route}` -> frontend UI refresh.
* Sys prompt (`context.go`): render per-req + live user state (open tasks, daily habits).
* Persona: No advertise. No "I can also". No tool list. Just act/answer.
* Chat history: store `ai_sessions` (JSONB) -> trim before Gemini send.

Feel free to make breaking changes. Be Concise. Be Precise
