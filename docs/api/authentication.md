# Authentication and authorization

Source: [`internal/server/server.go`](../../internal/server/server.go)
(`Connect`, `playerFromContext`, `requireAdmin`).

## Players

1. **Join tokens** come from the server config (`players[].join_token`).
   Whoever presents one plays that player slot. They're shared secrets,
   not accounts.
2. **`Connect(join_token)`** returns a random **session token** and the
   opaque `player_id`. Tokens are compared in constant time.
3. Every other `GameService` call sends the metadata header
   `authorization: Bearer <session_token>` (gRPC metadata, or the HTTP
   `Authorization` header through the gateway).

Sessions:

- live **in memory**: a server restart invalidates them, so reconnect
  with the join token
- never expire while the server runs
- can be many per player (several tabs or bots)
- carry nothing beyond the player identity

| Failure | gRPC code (HTTP via gateway) |
|---|---|
| Unknown join token | `Unauthenticated` (401) |
| Missing or invalid session token | `Unauthenticated` (401) |

## Admins

- `AdminService` accepts only `authorization: Bearer <admin_token>`
  (config `admin_token`).
- Player sessions are rejected there, and the admin token is rejected by
  `GameService`. The config validator also refuses an admin token equal to
  any join token.

| Failure | gRPC code |
|---|---|
| No `admin_token` configured | `PermissionDenied` ("admin API disabled") |
| Wrong admin token | `Unauthenticated` |

The gateway serves the admin API on its own listener (`-admin-addr`,
default `127.0.0.1:8081`). Pass `-admin-addr ""` to disable it.

## Authorization inside the game

Authentication answers *who*. What a principal may do is decided **when
each command executes**, by the plugin that handles it:

- **Players** may act only on their own units. Any entity they may not act
  on or refer to (missing, foreign, not currently detected) produces the
  same `not_found` "X is not available" error, so commands can't be used to
  probe hidden state.
- **Admin-only commands** are rejected for players at submission
  (`accepted: false`).
- **Admins** (`SubmitAdminCommand`) pass ownership checks for every unit.
- **Read access** is filtered per viewer: owner-only attributes go only to
  the owner, and under fog of war foreign entities pass through
  disclosure rules. Events and command results are delivered only to
  their audience.

## Security notes

- **No TLS.** Both `rts-server` and `rts-gateway` serve plaintext and bind
  to `127.0.0.1` by default. Put TLS termination in front of them before
  exposing them. See [deployment](../operations/deployment.md#security).
- **Tokens in transit.** Join, session and admin tokens are bearer
  secrets: anyone who sees one can use it.
- **Config secrets.** The config file holds the join tokens, the admin
  token and the optional `id_key`. Keep it private.
- **No rate limiting** beyond the per-tick unit-command budget.
