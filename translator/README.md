# Mattermost Translator

`io.github.sn-kinos.translator` is a Mattermost plugin for two server-side translation flows:

- messages from configured source users receive a Korean translation prepended to their own message, with the original English kept as a Markdown quote;
- `/trans <한국어 메시지>` privately shows three English choices with corresponding Korean sentences, then prepends the selected English text to the caller's source message, keeping the Korean original as a quote.

It requires Mattermost 11.3.0 or later and works on Team Edition. A small webapp bundle provides the System Console glossary editor; translation uses Mattermost's normal posts, ephemeral posts, slash commands, and interactive-message buttons, without a custom mobile client.

## Build and verify

Prerequisites:

- Go 1.27 or newer;
- Node.js 24 and npm for webapp tests (no bundler or runtime dependencies);
- `make`, `tar`, `rg`, and macOS-compatible `shasum`;
- Linux cross-compilation support from the Go toolchain. CGO is disabled.

Build the installation archive:

```sh
make dist
```

The result is `dist/io.github.sn-kinos.translator-0.1.0.tar.gz`. It contains `plugin.json`, Linux AMD64/ARM64 server binaries and `webapp/plugin.js`.

Run the complete release check:

```sh
make verify
```

This formats-checks the server code, runs vet, race and ordinary tests, scans for incomplete branches or skipped tests, inspects the archive, and builds it twice to compare SHA-256 hashes.

## Install

An administrator must complete these steps. Do not enable the plugin until the bot-account preconditions are satisfied.

1. In Mattermost, manually revoke every existing access token owned by `@translator`. The plugin API cannot enumerate all tokens and cannot verify this step.
2. Confirm `@translator` is active, is a bot, and has exactly the `system_user` role. It must not be a system administrator and must not have `system_post_all` or `system_post_all_public`.
3. In **System Console → Plugin Management**, upload `dist/io.github.sn-kinos.translator-0.1.0.tar.gz`.
4. Open the plugin's System Console settings, enter the Gemini API key and verify every setting below.
5. Save the settings, then enable the plugin. Invalid identity, role, team, source-user, credential, or model settings fail closed.

The plugin does not create access tokens, add `@translator` to channels, or grant roles. Mattermost's trusted server-plugin post API lets the ordinary bot publish without channel membership or Post-All permission.

## Configuration reference

| Setting | Type and default | Constraint and effect |
|---|---|---|
| Gemini API Key | secret text, no default | Required. Stored in Mattermost plugin configuration and used only by the server process. |
| Primary Model | text, `gemini-3.7-flash` | Required and validated with Gemini when settings are loaded. The client tries it twice for retryable failures. |
| Fallback Model | text, `gemini-3.6-flash` | Required and validated. Used after the two primary attempts. |
| Automatic Translation Team | text, empty | Required. Resolves to one active team. Public/private channel posts must belong to this team. |
| Automatic Source Users | text, empty | Required. Comma-separated usernames, with an optional leading `@`. Every entry must resolve to a distinct active user. |
| Include Direct Messages | boolean, `true` | Enables DMs and group DMs only when every active participant belongs to the configured team. |
| Shared Glossary | custom row editor with longtext fallback, empty | Korean/English fields with add/delete buttons. At most 200 unique Korean terms and 100 Unicode code points per side. Applies in both directions. Stored as one `Korean = English` pair per line, without migrating existing values. |
| Maximum Concurrent Gemini Requests | integer, `8` | Must be 1–64. One shared gate covers automatic and manual requests; this is process protection, not a user quota. |
| Total Request Timeout Seconds | integer, `10` | Must be `10` in this release. The primary, retry, and fallback attempts share this budget. |

설정 이름·설명과 사전 편집기의 버튼·오류 안내는 사용자의 Mattermost 언어 설정을 따릅니다. 한국어와 영어를 지원하며, 다른 언어는 영어로 표시합니다. **공용 단어 사전**에서 행별로 단어를 편집한 뒤 시스템 콘솔 아래의 **저장** 버튼을 누르세요. 중복 한국어, 미완성 단어 쌍, 길이 초과, 등호 입력은 저장 전에 안내합니다. 플러그인이 비활성화되면 영어로 된 기본 설정 입력창이 표시됩니다.

Initial automatic translation starts at plugin activation; there is no historical backfill. Each configured source message is treated as English without language detection.

## Use

For eligible source messages, the plugin updates the original message and prepends the Korean translation before one blank line. The original English stays as a Markdown quote, with no label or translator attribution. Root posts and thread replies stay in their original positions. A later source edit replaces the owned translation block. Uploaded files stay attached: the layout change never clears `FileIds`.

Any active user can run:

```text
/trans 검토해 주세요
```

The message is posted immediately under the caller's name with a `Translating…` head over the quoted Korean original. Once Gemini returns a natural English translation, the plugin edits the head into the translation:

```text
Could you review this?

> 검토해 주세요
```

Notifications and previews may show the `Translating…` version, since they reflect the original post. If translation fails, the post is restored to the plain Korean message and the caller gets a private notice. If the caller edits or deletes the post before the translation lands, their change wins.

Mattermost does not pass draft attachments to slash commands, so in the web and desktop apps the plugin's webapp bundle intercepts `/trans` when the draft has files and sends the message with its file IDs to the plugin's `POST /plugins/io.github.sn-kinos.translator/trans` endpoint. The files stay attached to the posted message. The mobile app runs `/trans` as a plain slash command, so attachments there are still dropped.

## Data sent to Gemini

Automatic English-to-Korean requests contain only:

- the source post's message text;
- textual attachment `pretext`, `title`, `text`, and field title/value content;
- for a thread reply, the thread root's corresponding text as parent context;
- the configured glossary, represented as English-source to required-Korean pairs.

Before transmission, Markdown code, user-written URLs, and mentions are replaced with reversible integrity tokens. Those tokens contain encoded original text so Gemini can return them unchanged. Treat the underlying inline code, URLs, and mention names as data disclosed to Gemini.

Manual Korean-to-English requests contain:

- the Korean message supplied to `/trans`;
- the thread root message when the command is run in a thread;
- the configured glossary, represented as Korean-source to required-English pairs.

The plugin does not read or send image pixels, uploaded file contents, PDFs, filenames, file IDs, attachment image/thumbnail/download URLs, surrounding channel history, access tokens, or the Gemini API key as prompt content. Gemini tool use and SDK retries are disabled; the plugin performs its own bounded retry and validates JSON-schema output.

## Storage, retention, and logs

Permanent translations are stored in the original Mattermost message and remain after plugin disablement or removal.

Mattermost plugin KV stores automatic job/mapping IDs, versions, state, timestamps, categorized errors, deletion intent, and publication recovery records. It does not duplicate automatic source bodies. Manual `/trans` translations store nothing in KV.

Operational logs are metadata-only: correlation and Mattermost IDs, operation, model, latency, attempt, and categorized outcome. Logs exclude message bodies, translations, API keys, KV values, and raw Gemini responses.

## Disable, uninstall, or roll back

To stop new work without removing existing messages, disable `io.github.sn-kinos.translator` in System Console. Deactivation drains in-flight work and leaves durable jobs, mappings, and already-appended translations intact.

To uninstall, disable the plugin and remove it from **System Console → Plugin Management**. Existing inline translations and historical `@translator` posts are not removed automatically.

To roll back code while retaining state and posts, disable this version, upload the previously approved server-only archive with a compatible plugin ID/state format, and re-enable it. If no compatible archive is available, leave the plugin disabled.
