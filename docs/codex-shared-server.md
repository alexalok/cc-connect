# Shared Codex app server

When another Codex client already owns a conversation, starting a private
app-server against the same profile can fail with `already has an active writer`.
Connect CC Connect to that client's existing local server instead:

```toml
[projects.agent.options]
backend = "app_server"
mode = "inherit"
app_server_url = "unix:///home/you/.codex/app-server-control/app-server-control.sock"
codex_home = "/home/you/.codex"
work_dir = "/path/to/project"
```

Use the actual absolute control socket path for your Codex profile. The server
must already be running and support JSON-RPC over WebSocket frames on a Unix
socket. This mode has been contract-tested with Codex 0.164.0 on Linux. CC Connect
does not start a daemon, elect a leader, remove locks, or fall back to a private
server when this endpoint is unavailable. Desktop and CLI must use the same
server for this to prevent writer conflicts; sharing `CODEX_HOME` alone is
insufficient. Keep the socket accessible only to the profile's OS user.

With no URL configured, `app_server` continues to launch a dedicated stdio
child. Supported URL forms are `stdio://` and `unix:///absolute/path`. TCP
WebSocket listeners are not supported by this adapter.

## Settings and lifecycle

- Each conversation uses its own connection to the same server. New threads
  receive `work_dir` explicitly. Resuming sends only the thread ID, preserving
  the existing thread's working directory and approval/sandbox settings.
- Shared mode requires an explicit `mode = "inherit"`. The native server controls
  approval and sandbox policies for both new and existing threads. CC Connect
  adds no policy restriction. Configurations claiming `suggest`, `full-auto`, or
  `yolo` are rejected; `/mode` displays only **Native policy** and cannot change
  it. Configure policy in the native client/server before granting users access.
  Explicit `model` and `reasoning_effort` options apply to submitted turns; leave
  them unset to keep the server's current values.
- The daemon owns its provider, credentials and environment. Per-project `env`,
  extra CLI arguments and provider overrides are rejected in shared mode.
  Runtime `CC_*` session variables cannot be injected into an already running
  daemon; tools requiring those variables need separate integration.
- `codex_home` selects the profile used for local history/discovery. It must
  match the configured server; it does not change the server's profile.
- `/stop` interrupts the specific turn accepted for this connection. Idle
  cleanup, `/new`, and shutdown close the subscriber connection. They do not
  kill the server. Archiving and native skill discovery also use the existing
  server, without requiring a local Codex executable.

## Multiple clients and recovery

Live output and approvals are relayed only for the turn that accepted this
connection's input. Desktop-only turns remain with the existing history/topic
mirror, avoiding a second live copy. Submitting input during another client's
active turn can join that turn (verified on 0.164.0); subsequent steering targets
its exact ID. Dynamic tools remain with the client that registered them.

A resolved approval releases the Telegram wait even when another client answered
it. Stale replies are rejected. Approval prompts issued before this connection
submitted input remain with their original client.

If a connection fails, its pending requests fail and the session becomes dead;
the next message can attach again. CC Connect never automatically replays a
submission whose acknowledgement was lost. The error warns that it may already
have been accepted: inspect the saved conversation before retrying. Stable
client message IDs provide attribution, not a server-side exactly-once guarantee.
Output generated while disconnected may need to be read from native history;
this mode does not promise automatic replay of missed output. Event buffers are
bounded and a subscriber that overflows is disconnected with an error.

## Validation and upgrades

The regular Go suite includes a Unix/WebSocket fault-injection peer covering
settings, early output, thread/turn isolation, approvals, targeted interruption,
archive, disconnect and ambiguous acceptance. The engine CUJ test covers an
approval answered elsewhere followed by another message in the same chat.

Before upgrading a managed Codex daemon, run the opt-in contract test against
that exact binary:

```sh
CC_CONNECT_TEST_CODEX_BIN=/absolute/path/to/codex \
  go test ./agent/codex -run '^TestSharedServerRealCodexContract$' -v
```

This launches a separate server with a disposable profile and local fake model
endpoint, without real credentials or model calls. It checks two subscribers,
idle/active resume, output isolation, reconnect and targeted interruption. The
server must be allowed to create its local socket. Review protocol changes when
this test fails; the shared app-server API includes experimental fields.
