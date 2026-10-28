# msg

A minimal XMPP messaging CLI. Run `msg` for commands.

## Unified account configuration

Put all accounts in `~/.config/msg/config.json` (or
`$XDG_CONFIG_HOME/msg/config.json`):

```json
{
  "accounts": {
    "default": {
      "jid": "bot@example.com",
      "password": "replace-me",
      "owner": "human@example.com"
    },
    "droid": {
      "jid": "droid@example.com",
      "password": "another-secret",
      "owner": "human@example.com",
      "service": "xmpp://chat.example.com:5222",
      "room": "testing@muc.example.com",
      "nick": "droid",
      "uploadService": "upload.example.com"
    }
  },
  "accountAliases": {
    "robot": "droid"
  }
}
```

Protect this file with `chmod 600`; it contains plaintext passwords. Do not
commit real configuration to git.

- `msg send "hello"` selects `default`.
- `msg --as droid room "hello"` selects `droid`.
- `msg --as robot room "hello"` selects the same account and runtime files.
- Explicit unknown names fail for messaging/listening; they never select
  `default` instead.
- Accounts are independent: credentials and optional settings do not inherit
  from `default`. Only `jid` and `password` are required (as with legacy msg).
- Field names `jid`, `password`, `owner`, `service`, `nick`, `uploadService`, and
  the top-level `accounts`/`accountAliases` follow pi-msg. Aliases must target
  real accounts, not other aliases, and cannot shadow accounts.
- `owner` is msg's default direct recipient (legacy `XMPP_TO`). `service` is
  the optional host:port override (legacy `XMPP_SERVER`); an `xmpp://` prefix
  is stripped, and `wss://…` selects XMPP-over-WebSocket.
- msg-specific `room` is a **single** MUC JID; pi-msg's multi-room routing and
  other bridge settings are not implemented by this CLI. `websocketUrl`
  (legacy `XMPP_WEBSOCKET_URL`) can explicitly override the WebSocket URL.
- `nick` defaults to the account JID's local part. An absent service retains
  the existing JID-domain:5222 connection default.

### Location, precedence, and legacy compatibility

`MSG_CONFIG=/path/to/accounts.json` explicitly selects a JSON file. Missing or
invalid explicit files fail rather than falling back to another account.

Otherwise msg looks for `config.json` or legacy dotenv files in the existing
order:

1. `MSG_DIR`, when set (no discovery elsewhere).
2. `$XDG_CONFIG_HOME/msg` or `~/.config/msg`.
3. Current working directory.
4. Running binary's directory.
5. `~/projects/msg`.

The first qualifying directory wins. Within it, JSON is authoritative: no
per-account merging or fallback to dotenv when JSON is present, malformed,
unreadable, a broken symlink, or missing a requested account. Valid JSON symlinks
are supported. If there is **no JSON entry**, the original `.env`
and `.env.<account>` loading behavior is unchanged.

Runtime files stay in the discovered directory. With `MSG_CONFIG`, they default
to its parent directory, unless `MSG_DIR` separately overrides state storage.
Named accounts retain `inbox.<account>.jsonl`, cursors, pidfiles, logs, etc.
Aliases use the canonical account's filenames. The JSON `default` account
(including explicit/aliased selection) uses the historical unnamespaced files.
No files or credentials are migrated automatically.

`status` and `stop` retain pidfile-based recovery when configuration is broken
or an account was removed: they warn and use the requested runtime filename.
Use the canonical `--as` name in that case, since aliases need valid JSON to
resolve. A valid alias still selects the canonical daemon's pidfile.

Legacy field mapping:

| Dotenv | JSON account field |
| --- | --- |
| `XMPP_JID` | `jid` |
| `XMPP_PASSWORD` | `password` |
| `XMPP_TO` | `owner` |
| `XMPP_SERVER` | `service` |
| `XMPP_ROOM` | `room` |
| `XMPP_NICK` | `nick` |
| `XMPP_UPLOAD_SERVICE` | `uploadService` |
| `XMPP_WEBSOCKET_URL` | `websocketUrl` |

### Registration

Shared ejabberd admin credentials (`XMPP_API_URL`, `XMPP_API_USER`,
`XMPP_API_PASSWORD`, optional `XMPP_API_HOST`) still come from the **base `.env`**
in the state directory, independent of `--as`. Account JSON does not change
this admin resource.

`msg register <name> [password]` adds credentials to the existing JSON accounts
object when present, preserving other accounts and unknown settings. Existing
accounts and aliases are not overwritten. Before contacting the API, msg stages
the updated JSON in a mode-0600 `.msg-config-*` file beside the config. A local
preparation failure makes no API request. If provisioning fails (including an
uncertain remote outcome) or the final rename fails, this recovery file is
retained and its path is reported, never the password. Keep it protected; verify
the remote account before retrying, then explicitly recover its credentials or
install the staged config after checking for intervening edits. Successful
rename consumes the staged file; the replacement has mode 0600.
Without JSON, registration still writes `.env.<name>`. Set `owner` (or legacy
`XMPP_TO`) before sending without an explicit recipient. Configuration updates
are atomic, but are not locked against simultaneous external writers; serialize
registration/config edits.

## Idle presence and last activity

The listen daemon supports [XEP-0319](https://xmpp.org/extensions/xep-0319.html)
idle presence and [XEP-0012](https://xmpp.org/extensions/xep-0012.html)
last-activity queries for its **online full JID** (including the bound resource).
It advertises `jabber:iq:last` in disco#info responses.

- `msg check` and successful `send`, `room`, `typing`, `send-file`, `room-file`,
  and `react` commands record actual activity in the account-scoped
  `last_activity` runtime file (aliases share the canonical account's clock).
- After 180 seconds without activity, presence includes
  `<idle xmlns="urn:xmpp:idle:1" since="…"/>`, using the last activity's UTC
  timestamp. The next presence refresh (every 20 seconds) removes it after
  renewed activity. Refreshes retain the original `since` value.
- `msg check --in` controls the existing available/extended-away poll status,
  **not** the activity timestamp. An available resource can still be idle.
- An online-resource IQ-get with `<query xmlns="jabber:iq:last"/>` returns
  elapsed whole seconds from the same clock. Receiving messages, automatic
  receipts/announcements, background traffic, and reconnects do not reset it.
- If no valid activity record exists (including invalid/future records), the
  daemon uses its startup time, retained across reconnects. This is a CLI/agent
  activity clock, not an operating-system keyboard/mouse idle monitor.

### Privacy and server requirements

Use `msg listen --no-last-activity` (also with `--background`) to disable
**both** idle presence and last-activity responses for this listener. Queries
then receive `service-unavailable`, and disco no longer advertises the feature.
Restart the daemon with the flag to change an already-running listener.

**The XMPP server MUST enforce presence subscriptions, privacy lists
(XEP-0016), and blocking (XEP-0191) before forwarding last-activity queries,
including queries to full JIDs.** Unauthorized queries must not reach msg;
XEP-0012 specifies `forbidden` for lack of presence authorization. The client
relies on this server boundary; it cannot infer those permissions from a
sender's JID or shared MUC membership. The existing automatic acceptance of
incoming presence subscriptions is unchanged: use server privacy/blocking
controls or the listener opt-out if you do not want to share activity.

msg does not provide offline last-seen, server uptime, or bare-JID last-activity
answers; these belong to the server. Check your server's enforcement before
enabling sharing on an account. Local tests cover client emission, request/
response framing, opt-out, and errors; they do not certify a live server's
subscription/privacy/blocking configuration.

## Development

```sh
nix develop --command go test ./...
nix develop --command go vet ./...
nix develop --command go build -o /tmp/msg .
```
