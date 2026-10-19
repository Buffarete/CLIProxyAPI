# Buff Cloud fork of CLI Proxy API

This fork is the backend of Buff Cloud's AI proxy. Buff Cloud (github.com/Buffarete/buff-cloud) is the only
dashboard: its AI page shows what this proxy knows and writes this proxy's settings. The upstream management
panel is disabled. Upstream is router-for-me/CLIProxyAPI; keep the fork rebasable on it, so changes live in
new files or small, clearly marked hooks, with tests.

## What Will's setup is

- One Linux box at home (box, residential IP, no screen) runs the proxy. Every other machine reaches it over
  Tailscale at https://box.tail042450.ts.net:14110. The service is a user systemd unit; a deploy script in
  buff-cloud builds this fork and restarts it.
- Seven Claude Code logins already exist on box as config dirs `~/.claude` and `~/.claude_2` to `~/.claude_7`
  (`.credentials.json` with `claudeAiOauth`). One Codex login at `~/.codex/auth.json`. T3 Code and routines
  on box keep using those dirs directly, so the proxy must share those credentials, not copy them: read the
  token from the dir's file on each use, refresh under Claude Code's own lock (`<dir>/.oauth_refresh.lock`
  then `<realpath(dir)>.lock`, proper-lockfile style directories, stale after 60 s), write back atomically
  in the file's own shape. The TypeScript reference for this is `proxy/accounts.ts` in the buff-cloud repo
  (before it is deleted) and `docs/decisions/ai-proxy.tsv` there records what was verified live.
- Clients: Claude Code and Codex get the full pool. pi and opencode may use the Codex path only. Anthropic
  refuses subscription logins for clients other than Claude Code (bare 429, no rate-limit headers; verified
  2026-10-19), and this fork never disguises a client: no Claude Code fingerprint, billing header, fake user
  id or system-prompt replacement is ever added to a request that did not carry it. The upstream cloaking
  code must be off and proven off by capturing outgoing requests. Codex cloaking likewise off.

## Phases, one PR each on this fork, then one in buff-cloud

### F1. Linked credentials, cloaking off, running on box

- A new credential source: an auth file of type `claude` or `codex` may carry `"linked_dir": "<path>"`. For
  such a file the token comes from the dir's own credentials file on every use, refresh happens under the
  dir's lock and is written back there, and nothing secret is persisted in `auths/`. A flag or management
  call `--link-claude-dir <path>` / `--link-codex-home <path>` creates the auth file (label, email from the
  dir's `.claude.json` oauthAccount if present, plan from `subscriptionType` or the Codex id_token claim).
- Config for box: `server.host 127.0.0.1`, port 4120 while the old proxy still owns 4110; `management`
  enabled with a key, panel disabled; `routing.session-affinity true`; `upstream.claude.disable-claude-cloak-mode
  true`, `upstream.codex.disable-codex-cloaking true`; request-log on. Prove with a dump server that a
  Claude Code request leaves the proxy with the same headers and body it arrived with, only the credential
  swapped. Same for Codex.
- Run as `buff-cli-proxy.service`; real `claude -p` and `codex exec` through it.

### F2. Routing by room left

- Two strategies beside upstream's: `most-room` (account whose worst window has the most room, ties to least
  recently used) and `soonest-reset` (account whose weekly reset is nearest, Theo's rule), both reading the
  signals `quota_signals.go` already observes (`anthropic-ratelimit-unified-{5h,7d}-{utilization,reset,status}`,
  `x-codex-{primary,secondary}-{used-percent,reset-at}`), plus a `pinned` credential id. Session affinity
  keeps a thread on its account. Unit tests for each rule, and a live demonstration.
- Codex WebSocket transport on, if it works with the real Codex CLI through the proxy.

### F3. What Buff Cloud reads and writes

- Management endpoints (or files) that give Buff Cloud, per account: label, provider, plan, enabled, room
  per window with reset times, cooldown, last status, token expiry, requests and tokens (input, output, cache
  read, cache write) in the last hour and today, and the last request. Plus Codex reset information if the
  ChatGPT backend exposes it. Plus a per-request usage log in the schema buff-cloud already reads
  (`proxy/types.ts` `UsageRecord`: at, accountId, provider, model, status, ms, tokens, sessionKey).
- Settings Buff Cloud writes through the management API: strategy, pinned id, fallback on/off, per-account
  enabled, proxy on/off.

### B1. buff-cloud switches over

- The AI page and its controls read from and write to this fork's management API; the TypeScript `proxy/`
  directory, its tests and `deploy/buff-ai-proxy.service` are deleted; `scripts/deploy-proxy.sh` builds this
  fork (Go toolchain at `~/.local/go`) and installs `buff-cli-proxy.service` on 127.0.0.1:4110 behind
  `tailscale serve --https=14110`; Connect snippets change to the upstream paths (`/v1/messages`,
  `/v1/responses`). The page keeps Buff Cloud's own look and shows at least: per-account room bars and reset
  times, cooldowns, plan, enabled switch, routing mode with pinned picker, fallback, requests and tokens by
  hour and by account with cache read/write split, last request, service status, and Codex resets.

## Rules

- Secrets never in logs, git, or the decision trail. Log counts, statuses and ids.
- One row per non-obvious decision in `docs/buff-cloud/decisions.tsv` (what, why, evidence, result).
- Prove each phase against the real tools on box, not only unit tests.
- `go build ./...`, `go vet ./...` and `go test ./...` for touched packages pass before a PR.
