# Usage page: proxy API contract (P1–P3)

Both repos build against this. The fork is Buffarete/CLIProxyAPI. All routes are under `/v8/management` and need
the management key. Everything here is additive: the current Buff Cloud on `main` must keep working with the
new proxy, and the new Buff Cloud must keep working with the old proxy (it feature-detects).

## Feature detection

`GET /buff/accounts` adds `service.features: string[]`. Missing means an old proxy.

| Feature | Step | Meaning |
|---|---|---|
| `usage-range` | P1 | `/buff/usage` takes `until`, filters, `limit`, `cursor`; `/buff/usage/series` exists; daily files |
| `usage-fields` | P2 | Rows carry `error`, `ttftMs`, `tokens.reasoning`, `stream`, `requestedModel`, `endpoint`; rows with no account |
| `usage-client` | P3 | Rows carry `client`, `clientVersion`, `routine`, `thread` |

## Row (one per upstream attempt, as today)

Existing fields are unchanged: `at` (ms), `accountId`, `provider`, `model`, `status`, `ms`, `tokens`
(`{input, output, cacheRead, cacheWrite}` or null), `sessionKey`, `traceId`.

New optional fields (omitted when empty, so old parsers are unaffected):

| Field | Type | Notes |
|---|---|---|
| `error` | string | `Fail.Body` cut to 300 chars, only when status ≥ 400. Secrets are not expected there; redact `sk-…`-like tokens anyway |
| `ttftMs` | number | Time to first token |
| `tokens.reasoning` | number | Inside `tokens` |
| `stream` | bool | |
| `requestedModel` | string | The model the client asked for, when it differs from `model` |
| `endpoint` | string | e.g. `/v1/messages` |
| `client` | string | Short name from User-Agent: `Claude Code`, `Codex`, `opencode`, `pi`, else the first UA product token |
| `clientVersion` | string | Version from User-Agent, when found |
| `routine` | string | From header `X-Buff-Routine` (trimmed, max 120 chars) |
| `thread` | string | From header `X-Buff-Thread` (trimmed, max 120 chars) |

Rows for requests that never reached an account are written with `accountId: ""` (P2).
The client IP is never stored.

**Amendment: unrouted rows are opt-in.** Rows with `accountId: ""` are written to the files, but
`/buff/usage`, `/buff/usage/series` and `/buff/summary` leave them out unless the request has `unrouted=1`.
The current Buff Cloud groups by `accountId`, so it never sees them.

## Storage (P1)

- New rows go to `usage/YYYY-MM-DD.jsonl` (box local day) next to the config file.
- Files older than 180 days are deleted (`BUFF_USAGE_KEEP_DAYS` overrides).
- The old single `usage.jsonl` is still read (as legacy, read-only) until its rows age past the window. It is not
  rewritten or deleted.
- Startup seeding (last row per account, last traces) works across both.

## `GET /buff/usage`

Without new params: the same as today (`{since, rows}`, all rows since `since`, oldest first).

New params:

| Param | Meaning |
|---|---|
| `until` | ms, exclusive. Default now |
| `account`, `provider`, `model`, `client`, `routine`, `thread`, `session` | Exact match. Repeat a param or comma-separate for several values. Prefix a value with `!` to exclude |
| `status` | `ok` (< 400), `error` (≥ 400 and not 429), `429`, or a number |
| `limit` | Max rows (≤ 5000). When set, rows are **newest first** and the reply adds `next` (cursor) when more remain |
| `cursor` | Opaque, from `next` |
| `unrouted` | `1` to include rows with `accountId: ""` (see the amendment above) |

Reply: `{since, until, rows, next?}`.

## `GET /buff/usage/series`

Params: `since`, `until`, the filters above, `bucket` = `5m` / `hour` / `day` / `week`, `groupBy` = `none` /
`account` / `model` / `provider` / `client` / `routine` / `thread` / `status`, `tz` (IANA, default the box's zone;
day and week buckets start at local midnight, weeks on Monday).

Reply:

```json
{
  "since": 0, "until": 0, "bucket": "day", "groupBy": "account", "tz": "Europe/London",
  "buckets": [
    { "start": 0, "groups": [
      { "key": "claude-7", "requests": 0, "errors": 0, "rateLimited": 0,
        "tokens": { "input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0, "reasoning": 0 },
        "msP50": 0, "msP95": 0, "ttftP50": 0, "models": { "claude-opus-5-5": { "requests": 0, "tokens": { "input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0, "reasoning": 0 } } } }
    ] }
  ]
}
```

- `errors` counts status ≥ 400 except 429; `rateLimited` counts 429.
- `models` is the per-model split inside each group, so Buff Cloud can price tokens per model (cost is computed in
  Buff Cloud from the LiteLLM table, never in the proxy).
- Empty buckets are omitted. Max 2,000 buckets per reply (400 otherwise).

## Headers (P3)

Clients may send `X-Buff-Routine: <routine name or id>` and `X-Buff-Thread: <T3 thread id or title>`. The proxy
does not forward them upstream.
