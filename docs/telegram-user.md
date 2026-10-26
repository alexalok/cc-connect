# Telegram user account setup

The `telegram_user` platform runs CC-Connect through a regular Telegram account.
Allowed people message that account, and replies are sent as the account using
MTProto. No BotFather token, public IP, or webhook is needed.

## Configure the account

Create an application at [my.telegram.org](https://my.telegram.org) under **API
development tools** to obtain `api_id` and `api_hash`. See Telegram's
[application setup instructions](https://core.telegram.org/api/obtaining_api_id).

Add the platform to your project's configuration:

```toml
[[projects]]
name = "telegram-assistant"

[projects.agent]
type = "codex"

[projects.agent.options]
work_dir = "/path/to/project"

[[projects.platforms]]
type = "telegram_user"

[projects.platforms.options]
api_id = 12345
api_hash = "${TELEGRAM_API_HASH}"
session_file = "~/.cc-connect/telegram-user.session.json"
allow_from = "123456789,987654321"

# Optional restriction to specific chats (in addition to the sender allowlist).
# Private chats use positive user IDs, basic groups use negative IDs,
# and supergroups use -100... IDs, as in the Telegram Bot API.
# allow_chats = "123456789,-1001234567890"

# Groups require an @mention or a mention by user ID by default.
# group_reply_all = false
# share_session_in_channel = false

# Maximum accepted messages per unique sender across ALL chats.
rate_limit_messages = 10
rate_limit_window_seconds = 60

# Optional SOCKS5 proxy for MTProto (HTTP proxies are not supported).
# proxy = "socks5://127.0.0.1:1080"
```

`allow_from` is required and accepts comma-separated numeric Telegram user IDs.
Use `"*"` only if you intend to give everyone access to the agent. An omitted or
empty allowlist is a configuration error. `allow_chats` narrows that access to
particular chats; it never grants access to additional senders.

The project-level `admin_from` setting controls privileged commands such as
`/shell` separately, just as with the other platforms.

## Log in once

Run this in an interactive terminal, using the same configuration and OS user
that will run the service:

```sh
cc-connect telegram-user login --config /absolute/path/config.toml --project telegram-assistant
```

Enter the phone number with its country code, the login code Telegram sends,
and the two-step verification password if the account uses one. Input is hidden.
The command supports existing accounts; it does not create a Telegram account.
`--project` is optional when the configuration contains exactly one
`telegram_user` platform.

Login writes an authorization session with owner-only permissions. Use an
absolute `session_file` path or one beginning with `~/`; this ensures the login
command and daemon find the same file. Keep that file private: it authorizes
access to the account. The login code and password are not saved in config.
Use each session file with only one running CC-Connect instance. Stop that
instance before logging in again.

Then start CC-Connect normally:

```sh
cc-connect --config /absolute/path/config.toml
```

Subsequent starts reuse the session without a prompt. If Telegram revokes it,
stop the service and repeat the login command. You can revoke the session from
Telegram's **Settings → Devices**.

## Message behavior

- Allowed private messages reach the configured agent. In groups, mention the
  account unless `group_reply_all = true`.
- The account's outgoing messages, Saved Messages, channel posts, anonymous
  senders, service messages, and Telegram login notifications are ignored.
- Each sender gets a separate conversation in each chat and forum topic.
  `share_session_in_channel = true` shares conversations within a group/topic.
- Replies stay in the original chat and forum topic. Long replies are split
  without losing text; shorter responses support streaming edits and typing.
- The default limit is **10 accepted messages per sender in any rolling 60-second
  window**, including commands and messages sent across different chats.
  Additional messages are silently discarded, not queued. They receive no
  automatic reply and do not extend the timeout. Processing resumes as earlier
  accepted messages leave the window. Restarting CC-Connect resets the counters.
  Both rate limit settings must be positive integers.
- This adapter handles text and media captions. Media attachments, voice
  transcription, bot command menus, and inline keyboards are not supported.
  Use text commands and text permission responses.
- Historical messages are not replayed on startup. Scheduled replies can resolve
  existing chat targets after a restart without waiting for a new message.

## Selective builds

The platform is included in normal builds. To include only this platform:

```sh
make build AGENTS=codex PLATFORMS_INCLUDE=telegram_user
```

To exclude it, use `EXCLUDE=telegram_user` or the `no_telegram_user` build tag.
The existing `telegram` Bot API platform remains separately selectable.
