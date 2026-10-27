# Buff Cloud management API

What Buff Cloud's AI page reads from and writes to this fork. Every path below is under
`/v8/management` and needs the management key, sent as `Authorization: Bearer <management-key>` or
`X-Management-Key: <management-key>`. On box the key is `CLI_PROXY_MANAGEMENT_KEY` in
`~/.config/buff-cloud/env` and the proxy listens on `127.0.0.1:4110` (4120 before buff-cloud B1).

Times are epoch milliseconds. Credential ids are auth file names, such as `linked-claude_4.json`.
Providers use Buff Cloud's names: `anthropic` for Claude credentials, `openai` for Codex.

The examples are real responses from box on 2026-10-19, with email addresses replaced.

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/buff/accounts` | Every credential with its rooms, cooldown, last request and token expiry; settings; service info. |
| GET | `/buff/usage?since=<ms>` | Usage rows since a time; filters and newest-first pages. |
| GET | `/buff/usage/series` | Usage counted into time buckets and groups. |
| GET | `/buff/summary?now=<ms>&dayStart=<ms>` | The AI page summary, in one call. |
| PATCH | `/buff/settings` | Strategy, pin, pin fallback, proxy on or off. |
| PATCH | `/credentials/status` | Enable or disable one credential (upstream endpoint). |
| GET | `/healthz` (not under `/v8/management`, no key) | Liveness for the status dot. |

No management response contains an access, refresh or id token. `POST /credentials/refresh`
answers with the same entry `GET /credentials` lists.

## GET /buff/accounts

One entry per credential, sorted by id, plus the routing settings and the running service.

```http
GET /v8/management/buff/accounts
Authorization: Bearer <management-key>
```

```json
{
  "settings": {
    "strategy": "most-room",
    "pinned": null,
    "pinnedFallback": true,
    "sessionAffinity": true,
    "sessionAffinityTTL": "1h",
    "proxyEnabled": true,
    "websocket": true
  },
  "service": {
    "version": "v8.0.34-24-gea9a9efe",
    "commit": "ea9a9efe51c594c499b970ab3d0560549cb50e46",
    "buildDate": "2026-10-19T15:42:56Z",
    "startedAt": 1792424581676,
    "features": ["usage-range"]
  },
  "accounts": [
    {
      "id": "linked-claude_4.json",
      "provider": "anthropic",
      "label": "Claude 4",
      "plan": "team",
      "enabled": true,
      "rooms": [
        { "window": "5h", "used": 0.24, "resetsAt": 1792438200000 },
        { "window": "7d", "used": 0.04, "resetsAt": 1792969200000 }
      ],
      "cooldownUntil": null,
      "lastStatus": 200,
      "email": "<email>",
      "linkedDir": "/home/will/.claude_4",
      "status": "active",
      "observedAt": 1792424509357,
      "lastUsedAt": 1792424503179,
      "tokenExpiresAt": 1792452345000
    },
    {
      "id": "linked-codex.json",
      "provider": "openai",
      "label": "Codex",
      "plan": "plus",
      "enabled": true,
      "rooms": [
        { "window": "primary", "used": 0.06, "resetsAt": 1792435070000 },
        { "window": "secondary", "used": 0.2, "resetsAt": 1792908107000 }
      ],
      "cooldownUntil": null,
      "lastStatus": 200,
      "email": "<email>",
      "linkedDir": "/home/will/.codex",
      "status": "active",
      "observedAt": 1792424585683,
      "lastUsedAt": 1792424583906,
      "tokenExpiresAt": 1793285030000,
      "websockets": true,
      "codex": {
        "signals": {
          "X-Codex-Credits-Balance": "59119.8157875000",
          "X-Codex-Plan-Type": "plus",
          "X-Codex-Primary-Reset-At": "1792435070",
          "X-Codex-Primary-Used-Percent": "6",
          "X-Codex-Primary-Window-Minutes": "300",
          "X-Codex-Secondary-Reset-At": "1792908107",
          "X-Codex-Secondary-Used-Percent": "20",
          "X-Codex-Secondary-Window-Minutes": "10080"
        },
        "usage": {
          "fetchedAt": 1792424737169,
          "fields": {
            "plan_type": "plus",
            "rate_limit": {
              "allowed": true,
              "limit_reached": false,
              "primary_window": { "used_percent": 6, "limit_window_seconds": 18000, "reset_after_seconds": 10333, "reset_at": 1792435070 },
              "secondary_window": { "used_percent": 20, "limit_window_seconds": 604800, "reset_after_seconds": 483370, "reset_at": 1792908107 }
            },
            "rate_limit_reset_credits": { "available_count": 2, "applicable_available_count": 0 },
            "credits": { "has_credits": true, "unlimited": false, "overage_limit_reached": false, "balance": "59119.8157875000",
                         "approx_local_messages": [14780, 76856], "approx_cloud_messages": [2365, 14780] },
            "model_usage": { "gpt-6-astra": { "available": true, "available_at": null, "credits_would_enable": false } },
            "spend_control": { "reached": false, "individual_limit": null },
            "rate_limit_reached_type": null,
            "promo": null,
            "code_review_rate_limit": null,
            "additional_rate_limits": null
          }
        }
      }
    }
  ]
}
```

The Codex `signals` example is shortened; the response lists every `x-codex-*` header the last response
carried.

Fields of each account:

- `rooms`: the rate-limit windows of the account's last response. Claude reports `5h` and `7d`, Codex
  `primary` (5 hours) and `secondary` (7 days). `used` is the fraction spent, 0 to 1. The list is empty
  until the proxy has had a response from the account. The router counts a window whose reset has
  passed as empty again; `/buff/summary` shows rooms that way.
- `cooldownUntil`: set while the account is cooling down after a limit or error.
- `lastStatus`, `lastUsedAt`: from the account's last row in the usage log (last 8 days).
- `tokenExpiresAt`: for a linked credential, read from the dir's own credentials file.
- `status`: upstream's `active`, `error` or `disabled`. `statusMessage` appears with errors.
- `codex.signals`: the `x-codex-*` headers of the last Codex response or WebSocket rate-limit event.
- `codex.usage`: the ChatGPT backend's `GET /backend-api/wham/usage`, the call Codex's `/status` makes.
  The proxy calls it at most once a minute per account. It passes on the quota fields and leaves out the
  account's email, user id and account id. `rate_limit_reset_credits.available_count` is the number of
  banked rate-limit resets; the headers do not carry it. When a call fails, `error` is set and `fields`
  keeps the previous answer.

`settings.websocket` is true when any Codex credential uses the WebSocket transport.

`service.features` names what this build adds for the Usage page (see `usage-contract.md`). A proxy without
the key is older than all of them.

| Feature | Meaning |
| --- | --- |
| `usage-range` | Day files, `/buff/usage` filters and pages, `/buff/usage/series`. |

## GET /buff/usage

The usage log's rows for requests that started at or after `since`, oldest first. `since` defaults to
24 hours ago. Rows with no account (`accountId: ""`) are left out unless `unrouted=1` is given.

The log is `usage/<YYYY-MM-DD>.jsonl` in the config file's directory (`~/.config/buff-cloud/cli-proxy/`):
one file per local day, by the day the request started. Day files older than 180 days go
(`BUFF_USAGE_KEEP_DAYS` changes that). The single `usage.jsonl` older builds wrote is still read, never
written or deleted; its rows drop out of replies as they pass the same window.

```http
GET /v8/management/buff/usage?since=1792424582157
Authorization: Bearer <management-key>
```

```json
{
  "since": 1792424582157,
  "rows": [
    {
      "at": 1792424582270, "accountId": "linked-codex.json", "provider": "openai", "model": "gpt-6.1-sol",
      "status": 200, "ms": 1001,
      "tokens": { "input": 9314, "output": 0, "cacheRead": 0, "cacheWrite": 0 },
      "sessionKey": "01a154d4-9c47-79b2-9a90-7a3a15a9bf51"
    },
    {
      "at": 1792424583906, "accountId": "linked-codex.json", "provider": "openai", "model": "gpt-6.1-sol",
      "status": 200, "ms": 1759,
      "tokens": { "input": 3916, "output": 5, "cacheRead": 9088, "cacheWrite": 0 },
      "sessionKey": "01a154d4-9c47-79b2-9a90-7a3a15a9bf51"
    }
  ]
}
```

Each row is one upstream request, in the schema of buff-cloud's `proxy/types.ts` `UsageRecord`:

- `at`: when the request started. The row is appended when the request finishes.
- `ms`: from start to the end of the response.
- `status`: 200, or the upstream or proxy error status. 499 means the client went away first.
- `tokens`: `null` when the response carried no usage. `input + output + cacheRead + cacheWrite` is the
  request's total for both providers; Codex's cached tokens are taken out of its `input_tokens`.
- `sessionKey`: the client's own session id, the name of the Claude Code transcript or Codex rollout file.
  A Claude Code subagent's requests carry the parent session's id.
- `traceId`: the inbound request's id. A messages row with the same `traceId` holds that request's
  messages (see `GET /buff/messages`).
- A retry on another account is a second row. A Codex WebSocket session starts with a prewarm request
  (`generate: false`, input tokens only), which is a row of its own; in the example it is the first row.

### Filters and pages

Any of these parameters turns on the extended reply `{since, until, rows, next?}`. Without them the reply
is exactly as above.

| Param | Meaning |
| --- | --- |
| `until` | Epoch ms, exclusive. Default now. |
| `account`, `provider`, `model`, `client`, `routine`, `thread`, `session` | Exact match. Repeat the param or comma-separate for several values. Prefix a value with `!` to exclude it. |
| `status` | `ok` (< 400), `error` (≥ 400 and not 429), `429`, or a number. Several and `!` work as above. |
| `unrouted` | `1` keeps rows with `accountId: ""`. |
| `limit` | At most 5000. Rows come **newest first**, and `next` is set when more remain. |
| `cursor` | The `next` of the previous page; needs `limit`. |

```http
GET /v8/management/buff/usage?since=1792364400000&account=!linked-codex.json&status=error,429&limit=100
```

```json
{ "since": 1792364400000, "until": 1792424596724, "rows": [ { "at": 1792424583906, "accountId": "linked-claude_4.json", "status": 429, "...": "..." } ], "next": "MTc5MjQyNDU4MzkwNi4x" }
```

## GET /buff/usage/series

The rows the filters above select, counted into time buckets and groups.

| Param | Meaning |
| --- | --- |
| `since`, `until` | Epoch ms. Default the last 24 hours. |
| filters | As for `/buff/usage`, `unrouted` included. |
| `bucket` | `5m`, `hour` (default), `day` or `week`. |
| `groupBy` | `none` (default; one group keyed `all`), `account`, `model`, `provider`, `client`, `routine`, `thread` or `status` (the code, such as `"429"`). |
| `tz` | IANA zone. Default the box's. Hours and 5 minutes follow the local clock; days start at local midnight, weeks on Monday. |

```http
GET /v8/management/buff/usage/series?since=1792364400000&bucket=day&groupBy=account&tz=Europe/London
```

```json
{
  "since": 1792364400000, "until": 1792424596724, "bucket": "day", "groupBy": "account", "tz": "Europe/London",
  "buckets": [
    { "start": 1792364400000, "groups": [
      { "key": "linked-claude_4.json", "requests": 2, "errors": 0, "rateLimited": 0,
        "tokens": { "input": 1174, "output": 19, "cacheRead": 0, "cacheWrite": 26207, "reasoning": 0 },
        "msP50": 1759, "msP95": 4120, "ttftP50": 0,
        "models": { "claude-opus-5-5": { "requests": 2, "tokens": { "input": 1174, "output": 19, "cacheRead": 0, "cacheWrite": 26207, "reasoning": 0 } } } }
    ] }
  ]
}
```

- `errors` counts status ≥ 400 except 429; `rateLimited` counts 429.
- `msP50`, `msP95` and `ttftP50` are nearest-rank percentiles over the group's rows. `ttftP50` counts only
  rows with `ttftMs` and is 0 when none has it.
- `models` splits each group by model (`unknown` for rows with none), so Buff Cloud can price tokens.
- Groups are sorted by requests, most first. Empty buckets are left out. A range of more than 2,000 buckets
  is refused with 400.

## GET /buff/messages

What each conversation request added, and the model's reply, from `messages/<YYYY-MM-DD>.jsonl` next to
the config file. Newest first, at most `limit` rows (200 at most). Files older than 90 days go
(`BUFF_MESSAGES_KEEP_DAYS` changes that). Requests to `/v1/messages`, `/v1/chat/completions` and
`/v1/responses` over HTTP are kept; Codex's WebSocket sessions are not.

```http
GET /v8/management/buff/messages?before=1792424590000&session=7f1c...&account=linked-claude.json&q=deploy&limit=50
Authorization: Bearer <management-key>
```

Every filter is optional: `before` (exclusive) and `since` are epoch ms, `traceId` picks one request,
`session` a client session, `account` an auth file, and `q` matches part text, tool names and the model.

```json
{
  "messages": [
    {
      "at": 1792424583906, "traceId": "01a17a52-d5a8-7e31-9f2b-478e313b7110", "path": "/v1/messages",
      "accountId": "linked-claude.json", "provider": "anthropic", "model": "claude-opus-5-5",
      "status": 200, "ms": 4120, "tokens": { "input": 12, "output": 80, "cacheRead": 90000, "cacheWrite": 400 },
      "sessionKey": "7f1c...",
      "input": [{ "kind": "tool_result", "role": "user", "text": "..." }, { "kind": "text", "role": "user", "text": "Now deploy it" }],
      "output": [{ "kind": "text", "role": "assistant", "text": "Deploying." }, { "kind": "tool_use", "role": "assistant", "name": "Bash", "text": "{\"command\":\"...\"}" }]
    }
  ],
  "more": true
}
```

- `input` is only what the client added since the model's last turn: the new user message, or the tool
  results it sends back. Earlier turns are in earlier rows of the same session.
- `kind` is `text`, `tool_use`, `tool_result`, `thinking` or `image` (an image is named, not kept).
- One part keeps at most 16 KB of text and a side at most 64 parts; `truncated` is set when anything was cut.
- Account, provider, tokens and the served model come from the request's usage row. A request that never
  reached an account has none of them. Requests refused with 401 or 403 are not kept.
- `more` is true when older rows match too; ask again with `before` set to the last row's `at`.

## GET /buff/summary

What buff-cloud's `src/lib/sources/ai-proxy.ts` `summariseProxy` computes, without `reachable`. Rows with no
account are not counted. Each count
also has its token `split`, and `todayByModel` counts today's rows by model (`unknown` when a row has
none). `now` defaults to the current time. `dayStart` defaults to the local midnight before `now`, in
the proxy's time zone. The last hour is rows that started at or after `now - 1 h`.

```http
GET /v8/management/buff/summary
Authorization: Bearer <management-key>
```

```json
{
  "now": 1792424596724,
  "dayStart": 1792364400000,
  "accounts": [
    {
      "id": "linked-claude_4.json", "provider": "anthropic", "label": "Claude 4", "plan": "team", "enabled": true,
      "rooms": [ { "window": "5h", "left": 0.76, "resetsAt": 1792438200000 }, { "window": "7d", "left": 0.96, "resetsAt": 1792969200000 } ],
      "cooldownUntil": null, "lastStatus": 200,
      "lastHour": { "requests": 2, "tokens": 27400, "split": { "input": 1174, "output": 19, "cacheRead": 0, "cacheWrite": 26207 } },
      "today": { "requests": 2, "tokens": 27400, "split": { "input": 1174, "output": 19, "cacheRead": 0, "cacheWrite": 26207 } }
    },
    {
      "id": "linked-codex.json", "provider": "openai", "label": "Codex", "plan": "plus", "enabled": true,
      "rooms": [ { "window": "5h", "left": 0.94, "resetsAt": 1792435070000 }, { "window": "7d", "left": 0.8, "resetsAt": 1792908107000 } ],
      "cooldownUntil": null, "lastStatus": 200,
      "lastHour": { "requests": 2, "tokens": 22323, "split": { "input": 13230, "output": 5, "cacheRead": 9088, "cacheWrite": 0 } },
      "today": { "requests": 2, "tokens": 22323, "split": { "input": 13230, "output": 5, "cacheRead": 9088, "cacheWrite": 0 } }
    }
  ],
  "lastHour": { "requests": 4, "tokens": 49723, "split": { "input": 14404, "output": 24, "cacheRead": 9088, "cacheWrite": 26207 } },
  "today": { "requests": 4, "tokens": 49723, "split": { "input": 14404, "output": 24, "cacheRead": 9088, "cacheWrite": 26207 } },
  "todayByModel": {
    "claude-opus-5-5": { "requests": 2, "tokens": 27400, "split": { "input": 1174, "output": 19, "cacheRead": 0, "cacheWrite": 26207 } },
    "gpt-6.1-sol": { "requests": 2, "tokens": 22323, "split": { "input": 13230, "output": 5, "cacheRead": 9088, "cacheWrite": 0 } }
  },
  "last": { "accountId": "linked-codex.json", "at": 1792424583906, "status": 200 }
}
```

The example shows two of the eight accounts. Rooms here are `left` (1 - used), with Codex windows named
`5h` and `7d`, and a reset that has passed shown as full with no `resetsAt`.

## PATCH /buff/settings

Changes any of `strategy`, `pinned`, `pinnedFallback` and `proxyEnabled`, writes them to `config.yaml`
(`routing.strategy`, `routing.pinned`, `routing.pinned-fallback`, `routing.paused`) and applies them
before answering. Nothing is written unless every field is valid. The answer is the new `settings` block.

| Field | Values |
| --- | --- |
| `strategy` | `most-room`, `soonest-reset`, `round-robin`, `weighted-round-robin`, `fill-first` |
| `pinned` | an enabled credential's id, or `null` or `""` to clear |
| `pinnedFallback` | `true`: other accounts serve the pinned provider when the pin cannot. `false`: the request gets the pinned account's error. |
| `proxyEnabled` | `false`: every proxied request gets a 403. Management and `/healthz` stay up. |

Change the strategy:

```http
PATCH /v8/management/buff/settings
Authorization: Bearer <management-key>
Content-Type: application/json

{"strategy": "soonest-reset"}
```

```json
{"strategy":"soonest-reset","pinned":null,"pinnedFallback":true,"sessionAffinity":true,"sessionAffinityTTL":"1h","proxyEnabled":true,"websocket":true}
```

Pin an account with fallback off:

```json
{"pinned": "linked-claude_6.json", "pinnedFallback": false}
```

```json
{"strategy":"soonest-reset","pinned":"linked-claude_6.json","pinnedFallback":false,"sessionAffinity":true,"sessionAffinityTTL":"1h","proxyEnabled":true,"websocket":true}
```

The pin applies to requests for the pinned account's provider only. Other requests route by the strategy.

Clear the pin and go back to most room:

```json
{"pinned": null, "pinnedFallback": true, "strategy": "most-room"}
```

Rejected writes answer 400 and change nothing:

```json
{"error":"no credential \"linked-claude_9.json\"; use an id from GET /v8/management/buff/accounts"}
{"error":"credential \"linked-claude_2.json\" is disabled"}
{"error":"unknown strategy \"cheapest\"; use most-room, soonest-reset, round-robin, weighted-round-robin or fill-first"}
```

Turn the proxy off with `{"proxyEnabled": false}` and on with `{"proxyEnabled": true}`. While it is off,
`/v1/messages`, `/v1/responses` and every other proxied path answer:

```http
HTTP/1.1 403 Forbidden

{"error":{"message":"Buff Cloud has turned this proxy off. Turn it back on from the AI page.","type":"proxy_off"},"type":"error"}
```

It is a 403, not a 503, because Claude Code treats a 503 as transient and retries it: with a 503,
`claude -p` printed nothing for 90 seconds while the proxy was off (19 attempts) before it was stopped.
A 403 ends the request at once and the client shows the message.

## Enable or disable a credential

Use upstream's endpoint. For a linked credential it writes `disabled` into the auth file, which holds no
tokens.

```http
PATCH /v8/management/credentials/status
Authorization: Bearer <management-key>
Content-Type: application/json

{"name": "linked-claude_2.json", "disabled": true}
```

```json
{"disabled":true,"status":"ok"}
```

`GET /buff/accounts` then shows the account with `"enabled": false` and `"status": "disabled"`. Send
`"disabled": false` to enable it again.

## Liveness

`GET /healthz` (and `HEAD /healthz`) on the proxy port needs no key and answers `{"status":"ok"}` while the
process serves HTTP, including while the proxy is turned off. There is no `/health`; the TypeScript proxy's
path changes to `/healthz`. `GET /buff/accounts` reports whether the proxy is turned off
(`settings.proxyEnabled`).
