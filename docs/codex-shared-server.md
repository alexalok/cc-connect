# Shared Codex app server

To continue conversations also opened in the Codex desktop or CLI, connect to
its existing local app server. Starting a separate server for the same thread
can fail with `already has an active writer`.

```toml
[projects.agent.options]
backend = "app_server"
app_server_url = "unix:///home/codex/.codex/app-server-control/app-server-control.sock"
```

Use the absolute control socket path for your Codex home. The server must already
be running. CC Connect uses WebSocket framing over this private Unix socket;
closing a bridge session does not stop the shared server. With no URL configured,
the existing dedicated stdio behavior remains the default.
