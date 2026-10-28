# Telegram Setup Guide

This guide walks you through connecting **cc-connect** to Telegram, so you can chat with your local Claude Code via a Telegram bot.

The native **typing…** indicator stays active during agent startup, thinking, and tool use. It refreshes every three seconds and after progress messages, resumes after temporary connection failures, and stops when the turn completes or is cancelled. This works independently for each chat topic and requires no configuration.

## Prerequisites

- A Telegram account
- A machine that can run cc-connect (no public IP needed)
- Claude Code installed and configured

> 💡 **Advantage**: Uses Long Polling mode — no public IP, no domain, no reverse proxy needed.

---

## Step 1: Create a Telegram Bot

### 1.1 Open BotFather

Search for **@BotFather** in Telegram (the official bot manager) and start a chat.

> ⚠️ Make sure it's the verified official BotFather — don't use third-party imitations.

### 1.2 Create a New Bot

Send the command `/newbot`. BotFather will ask you to provide a name and username.

### 1.3 Set the Bot Name

Enter a **display name** for your bot (e.g. `cc-connect`).

### 1.4 Set the Bot Username

Enter a **username** (must end with `bot`, e.g. `cc_connect_bot`).

> 💡 **Naming rules:**
> - Must end with `bot` (case-insensitive)
> - Only letters, numbers, and underscores
> - Must be globally unique

### 1.5 Get the Bot Token

After creation, BotFather will reply with something like:

```
Done! Congratulations on your new bot...
Use this token to access the HTTP API:
1234567890:ABCdefGHIjklMNOpqrsTUVwxyz-123456

Keep your token secure...
```

> ⚠️ Save this token immediately — it's only shown once! If lost, use `/mybots` → select bot → `API Token` → `Revoke current token` to regenerate.

---

## Message read indicators

With `read_receipts = true` (the default), an **👀 reaction on your original
message** means the agent confirmed receipt of that input. Codex supports this
with `backend = "app_server"`, after a successful `turn/start` acknowledgment.
Replies to its non-blocking questions are marked after `turn/steer` succeeds;
answers collected across several questions are marked together when delivered.

Messages waiting for startup, transcription, or a busy agent stay unmarked.
Rejected messages and failed sends do not receive the indicator. Voice messages
are marked on the original voice bubble after Codex accepts the transcription.
The reaction confirms receipt, not successful completion of the requested work.
Agents that cannot acknowledge input do not produce read indicators.

Telegram bots use [message reactions](https://core.telegram.org/bots/api#setmessagereaction)
for this indicator. Telegram's native read-marking API is limited to connected
business accounts. The chat must allow the 👀 reaction; a Telegram API failure
may prevent the indicator even when the agent received the message.

Set `read_receipts = false` under `[projects.platforms.options]` to disable it.
The older `enable_reactions = true` option adds ⚡ on arrival at the bridge and
only applies when read receipts are disabled.

## Step 2: Configure cc-connect

Add the token to your `config.toml`:

```toml
[[projects]]
name = "my-project"

# ── Project-level settings ──────────────────────────────────
# admin_from: who can run privileged commands (/shell, /restart, /upgrade).
#   Not set (default) → privileged commands are blocked for everyone.
#   "*" → all allowed users get admin access (only for personal single-user setups).
#   "id1,id2" → only these Telegram user IDs can run privileged commands.
admin_from = "*"

[projects.agent]
type = "claudecode"

[projects.agent.options]
work_dir = "/path/to/your/project"
mode = "default"

[[projects.platforms]]
type = "telegram"

[projects.platforms.options]
token = "1234567890:ABCdefGHIjklMNOpqrsTUVwxyz-123456"

# ── Platform-level settings ─────────────────────────────────
# allow_from: who can use this bot.
#   Not set (default) → all users are permitted (a WARN will be logged).
#   "*" → same as not set, but explicit (no WARN).
#   "id1,id2" → only these Telegram user IDs can interact with the bot.
# allow_from = "123456789"

# Optional proxy for Telegram Bot API access.
# Supports URLs like:
#   http://127.0.0.1:7890
#   socks5://127.0.0.1:1080
# proxy = "http://127.0.0.1:7890"
# proxy_username = ""
# proxy_password = ""
```

> **Common mistake:** `admin_from` goes under `[[projects]]` (project level), NOT inside `[projects.platforms.options]`. If placed in the wrong section, it will be silently ignored.
>
> To find your Telegram user ID, send any message to **@userinfobot**.

### 2.1 Optional: Use a Proxy

If your machine cannot reach the Telegram Bot API directly, set a forward proxy in the Telegram platform options:

```toml
[[projects.platforms]]
type = "telegram"

[projects.platforms.options]
token = "1234567890:ABCdefGHIjklMNOpqrsTUVwxyz-123456"
proxy = "socks5://127.0.0.1:1080"
proxy_username = ""
proxy_password = ""
```

Notes:

- `proxy` accepts HTTP or SOCKS5 proxy URLs.
- Leave `proxy_username` and `proxy_password` empty when the proxy does not require authentication.
- The proxy only affects Telegram Bot API requests for this Telegram platform instance.

---

## Step 3: Get Chat ID (Optional)

If you want to restrict the bot to specific users/groups, you'll need the Chat ID.

### 3.1 Get Your Personal Chat ID

1. Send any message to your bot
2. Visit the following URL (replace `{{TOKEN}}` with your token):

```
https://api.telegram.org/bot{{TOKEN}}/getUpdates
```

3. Find the `chat.id` field in the returned JSON

### 3.2 Get a Group Chat ID

1. Add the bot to a group
2. Send a message mentioning @your_bot in the group
3. Check the `getUpdates` URL — group Chat IDs are usually negative numbers

> Note: Chat ID whitelisting is planned for a future release.

---

## Step 4: Set Bot Commands (Optional)

### 4.1 Set Command Menu

In BotFather, send:

```
/setcommands
```

Select your bot, then enter the command list:

```
help - Show available commands
new - Start a new session
list - List sessions
```

### 4.2 Set Bot Description

```
/setdescription
```

Enter a description — users will see this when they first open the bot.

---

## Step 5: Start cc-connect

### 5.1 Launch

```bash
cc-connect
# Or specify a config file
cc-connect -config /path/to/config.toml
```

### 5.2 Verify Connection

You should see logs like:

```
level=INFO msg="telegram: connected" bot=cc_connect_bot
level=INFO msg="platform started" project=my-project platform=telegram
level=INFO msg="cc-connect is running" projects=1
```

### 5.3 Messages Sent During Downtime

On startup or reconnect, cc-connect skips pending messages sent before the connection's recovery cutoff. It does not run the agent on that backlog. Each affected chat or topic receives one summary containing the number of skipped messages, the earliest skipped timestamp (UTC), and a request to resend anything still relevant. Counts include authorized messages the bot would normally accept, including supported attachments; service events, button callbacks, and messages from unauthorized senders are excluded.

A failed polling request returns to the connection retry loop, so this recovery also runs after network outages while cc-connect stays running. Live updates fetched before the failure finish processing before reconnection; errors sending replies do not restart polling.

Updates already handled before a polling disconnect are acknowledged on recovery and excluded from skipped-message notices.

Messages arriving during recovery remain available for normal processing. If Telegram delivers an older message after the first live message, cc-connect skips it and updates the existing summary instead of sending another notice. Counts cover only updates Telegram still retains. Notices use the configured cc-connect language; delivery failures are logged without blocking the connection or repeatedly posting copies.

---

## Step 6: Start Chatting

### 6.1 Direct Message

1. Search for your bot's username in Telegram
2. Click "Start" to begin
3. Send a message

### 6.2 Group Chat

1. Create or open a group
2. Go to group settings → Add members
3. Search and add your bot
4. Send messages in the group

### 6.3 Topic Sessions

Telegram topics include a `message_thread_id`. cc-connect uses that thread ID
as part of the Telegram session key, so each topic has its own independent
conversation context. This applies to forum topics in groups and private chat
topics.

For private topics, enable Threaded mode and allow users to create topics in
BotFather. Sending an ordinary message from **All Messages** then creates a
topic in Telegram. cc-connect adopts that topic and uses the opening message
as its first input; no group, `/new` command, or second topic is needed. Images,
documents, voice notes, and audio can also be the opening input.

Some Telegram clients omit the thread ID on that opening message. cc-connect
uses the preceding topic-creation event from the same authorized human and
private chat to route messages sent within five seconds of that event or the
previous opener chunk. Explicit thread IDs always take precedence, and media
album parts retain their topic. This inference never applies to groups.

If multiple topics are created before their opening messages arrive, the
missing IDs can be ambiguous. Those messages are not sent to an agent;
cc-connect asks the sender to resend them inside the intended topic.

### 6.4 New Codex Chats as Telegram Topics

To automatically create a topic when you start a new Codex chat in the TUI
or desktop app, set `topic_sync_chat_id` in the Telegram platform options.
Use a user's ID for a DM or the group's ID for a supergroup. cc-connect
queries Telegram to detect the destination type; no separate type setting
is needed. Numeric strings and integers are accepted.

For a **DM**, enable private topics in BotFather and start a DM with the bot:

```toml
[[projects.platforms]]
type = "telegram"
[projects.platforms.options]
token = "YOUR_BOT_TOKEN"
allow_from = "123456789"
topic_sync_chat_id = "123456789" # Your private chat ID (also your user ID)
topic_sync_interval_seconds = 10
```

For a **supergroup with Topics enabled**, add the bot as an administrator
with **Manage Topics** permission and use the group's numeric ID (normally
starting with `-100`):

```toml
[[projects.platforms]]
type = "telegram"
[projects.platforms.options]
token = "YOUR_BOT_TOKEN"
allow_from = "123456789" # Users allowed to control the agent, not the group ID
topic_sync_chat_id = "-1001234567890"
topic_sync_interval_seconds = 10
group_reply_all = true # Optional: accept ordinary messages without @mention
```

Private topic mode in BotFather is not required for a supergroup. Before
creating a topic, cc-connect verifies that Topics are enabled and the bot
has the required administrator permission. Basic groups, channels, and
supergroups without Topics are rejected.

In the configured destination supergroup, each topic shares one conversation
among users permitted by `allow_from`, regardless of `share_session_in_channel`.
This includes messages, command buttons, and permission buttons, so another
allowed user's reply continues the same synced conversation. Other chats
keep their configured session-sharing behavior. Group members can see the
mirrored conversation; `allow_from` controls who can interact with the agent.

The destination is explicit: cc-connect never chooses one from incoming
messages. Changing the ID selects a separate sync journal and a new initial
cutoff; it does not move existing topics or copy previously linked chats.
Switching back to a previous destination resumes its saved links and delivery
progress. Existing DM configurations continue to work unchanged.

This is opt-in and works with the Codex agent in a single-workspace project.
Set `backend = "app_server"` under `[projects.agent.options]`; mirroring
uses its persisted turn-origin metadata to avoid echoing bot replies.
Set `list_all_sessions = true` under `[projects.agent.options]` to discover
chats across working directories; otherwise discovery is limited to the
configured `work_dir`. Clients must share the same `codex_home` / `CODEX_HOME`
on the bot's host. Chats stored on another machine are not discovered.

Only chats created after sync is first enabled are imported. The cutoff and
topic links are stored beside the project's session file, so normal restarts
retain existing links and catch up on chats created while the bot was offline.
Old chats that you resume or rename are not imported. Archived chats,
subagents, and chats already linked by cc-connect are excluded.

Each topic uses the Codex chat title (or its first prompt when available).
It receives the conversation's existing user messages and visible assistant
messages, then keeps receiving new ones at the configured polling interval.
Previously created empty sync topics catch up automatically after upgrading.
Internal instructions, reasoning, and tool output are excluded. Attachments
are represented by a note pointing to the original chat.
Replies resume that same chat in its saved working directory. `/history`
can read its native history before the first bot turn. Discovery does not
start a Codex turn. Completed messages are mirrored; token-by-token streaming,
message edits, and subsequent title or deletion changes are not
mirrored. `/new` or `/switch` in a topic stops delivery from its original chat.
Delivery progress is saved after each successful message chunk and survives
restarts. A crash between Telegram accepting a chunk and saving that progress
can replay the last chunk.

Archiving a linked Codex chat in another client removes its corresponding
Telegram topic, including the topic's messages, on the next poll. This also
applies to topics originally created through Telegram in `topic_sync_chat_id`.
The bridge checks Codex's persisted `archived_sessions` records, so it catches
archives made while offline and does not treat missing files as archives.
Codex's native archived history is preserved. Busy topics wait until their turn
finishes; topics reused with `/new` or `/switch` are only removed if their
currently selected conversation is archived. Failed Telegram deletions retry
on later polls, including after restarts. Successful deletions are remembered;
unarchiving a chat does not recreate its removed topic.
The General topic of a supergroup is never removed by archive sync.

Keep the session files and their `.topic-sync-*.json` companions together
when backing up or moving the bot. A malformed sync file stops startup
instead of silently resetting the cutoff and losing the saved links.

---

## Usage Example

```
User: Help me analyze the current project structure

cc-connect: 🤔 Thinking...
cc-connect: 🔧 Tool: Bash(ls -la)
cc-connect: Here's the project structure...
```

---

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                      Telegram Cloud                          │
│                                                              │
│   User Message ──→ Telegram Bot API ◄── Long Polling         │
│                          ▲                                   │
└──────────────────────────┼───────────────────────────────────┘
                           │
                           │ HTTPS (no public IP needed)
                           ▼
┌─────────────────────────────────────────────────────────────┐
│                    Your Local Machine                         │
│                                                              │
│   cc-connect ◄──► Claude Code CLI ◄──► Your Project Code    │
│                                                              │
└─────────────────────────────────────────────────────────────┘
```

---

## Long Polling vs Webhook

| Feature | Long Polling | Webhook |
|---------|-------------|---------|
| Public IP | ❌ Not needed | ✅ Required |
| Domain | ❌ Not needed | ✅ Required |
| HTTPS cert | ❌ Not needed | ✅ Required |
| Complexity | Simple | More complex |
| Latency | Low (long poll) | Low |
| Best for | Local dev, private network | Production |

---

## FAQ

### Q: Bot doesn't respond to messages?

Check the following:
1. Is cc-connect running?
2. Is the bot token correct?
3. Have you sent a message after starting cc-connect? (The bot only receives messages after startup)

### Q: How to regenerate the token?

1. Send `/mybots` to BotFather
2. Select your bot
3. Click `API Token` → `Revoke current token`

### Q: Bot doesn't respond in groups?

Make sure Group Privacy mode is disabled. In BotFather: `/mybots` → select bot → `Bot Settings` → `Group Privacy` → `Turn off`.

---

## References

- [Telegram Bot API Documentation](https://core.telegram.org/bots/api)
- [BotFather Guide](https://core.telegram.org/bots#botfather)
- [Telegram Bot Tutorial](https://core.telegram.org/bots/tutorial)

---

## See Also

- [Feishu Setup](./feishu.md)
- [DingTalk Setup](./dingtalk.md)
- [Weibo Setup](./weibo.md)
- [Slack Setup](./slack.md)
- [Discord Setup](./discord.md)
- [Back to README](../README.md)

## Topic status icons

Enable `topic_status_icons = true` in `[projects.platforms.options]` to show
agent turn status in private-chat topics and forum topics. Icons update as the
bridge processes turns; existing inactive topics are not retroactively labeled:

| Icon | Meaning |
| --- | --- |
| ⚡ | Working |
| ❓ | An answer is needed |
| 👀 | Approval is needed |
| ✅ | Turn finished |
| ❗ | Failed or timed out |
| 🏁 | Stopped |

The bot needs topic-management rights in groups. Topics retain their names.
An unanswered structured question keeps the question icon even if independent
work finishes. Questions written only as ordinary prose cannot be detected
reliably. Finished means a turn ended; it does not mean the response was read.

Each icon can be customized with `topic_status_<status>_emoji`, where status is
`working`, `question`, `approval`, `finished`, `failed`, or `stopped`. Choose an
emoji in Telegram's `getForumTopicIconStickers` catalog. Updates are coalesced,
serialized, and retried after rate limits without delaying chat messages.

The Bot API cannot reorder topics. Telegram's MTProto API can reorder pinned
topics through a logged-in user account, subject to Telegram's pin limit.

When Codex conversation mirroring is enabled, imported topics also use local
turn-start, completion, interruption and error events. Structured input requests
are shown while awaiting the next user input. This does not resume the native
chat or mark its messages as read. Native approval dialogs that are not persisted
in the transcript remain visible in their originating client.
