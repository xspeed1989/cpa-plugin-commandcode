# cpa-plugin-commandcode

CLIProxyAPI native provider plugin for [commandcode.ai](https://api.commandcode.ai) (`api.commandcode.ai/provider/v1`).

This plugin implements a full provider (`ModelProvider + ModelRouter +
Executor + Request/Response translators`) that calls CommandCode's **Responses
API** at `/provider/v1/responses`. It declares both `openai-response` and
`openai` as executor input/output formats. Responses clients keep native input,
response objects and typed SSE events, without a Responses → Chat → Responses
round-trip. Chat/Claude routes retain Chat Completions compatibility.

For chat output, reasoning is exposed as `reasoning_content`, which CLIProxyAPI's
built-in openai→claude translator requires. Streaming `/v1/messages` additionally
needs each chat chunk to arrive with exactly one SSE `data: ` prefix. The plugin
applies that route-specific framing without modifying CLIProxyAPI.

## Capabilities

- `model_provider` — advertises the configured models under the
  `commandcode/` namespace (so they never collide with the native
  openai-compatibility channel; the ABI has no live `/v1/models` discovery).
- `model_router` — routes only configured models with the `commandcode/`
  prefix (for example `commandcode/deepseek-flash`) to this executor.
- `executor` — POSTs to `/responses` through the host HTTP client
  (proxy policy + request-log preserved). Native Responses output stays native;
  chat output converts text, reasoning, function calls, finish reasons and usage
  to Chat Completions, with the endpoint-specific framing described below.
- `request_translator` / `response_translator` — model-name normalization for
  both input protocols, native Responses identity translation and legacy chat
  reasoning backfill.

## Protocol selection

The host selects input and output formats independently:

- `ExecutorRequest.SourceFormat` selects input handling. Native Responses input
  is preserved, including encrypted reasoning, previous response IDs, custom
  tools, structured output, extension fields and large JSON integers. Only the
  configured model alias and the execution method's `stream` flag are rewritten.
- `ExecutorRequest.Format` selects output handling. `openai-response` returns
  the original Responses object or typed SSE events; `openai` returns converted
  Chat Completions messages/chunks. Missing formats retain the legacy chat
  default (direct native calls can select Responses with `Format`).

Both streaming and non-streaming execution use upstream `/responses`. For chat
input, the executor converts `messages` into `input`, including `function_call`
and `function_call_output` items with matching `call_id` values. Function tools
and explicit function choices use the flat Responses schema; text/image/file
content parts, `reasoning_effort`, `response_format` and token limits are mapped
to their Responses equivalents. An omitted function-tool `strict` remains
`false`, preserving Chat Completions' default schema behavior. Chat-only fields
such as `messages`, `stream_options`, `n` and `stop` are not sent upstream; this
filter does not apply to native Responses input.

Model mappings and the API-key pool are unchanged. Since v0.3.7, clients must
add the `commandcode/` prefix to target this plugin; bare aliases fall through
to the host's normal routing. Native background response statuses are
preserved, but the plugin does not add retrieval/cancellation HTTP endpoints.

## Streaming compatibility

Native Responses output is emitted as complete `event: <type>\ndata: <JSON>\n\n`
SSE frames, preserving original JSON, sequence numbers, item IDs, annotations,
reasoning and custom/built-in tool events. Lifecycle and empty item-header events
are buffered until useful output or a successful terminal event is available,
so failed attempts cannot leak their response IDs before key failover. After
output starts, original terminal failure events are forwarded without replay.
A native stream ending without a terminal event is reported as an error.

For chat output, the executor reads the host's public `request_path` metadata
and prefixes a normalized chunk with exactly one `data: ` only when its value
is exactly `/v1/messages`. Chat Completions output stays bare for the Chat
Completions handler, which adds HTTP SSE framing itself. Missing, non-string,
unknown or near-match paths also remain bare (fail closed). Native Responses
framing follows `Format`, not this endpoint metadata.

The plugin strips stacked upstream `data:` prefixes and swallows `[DONE]`.
Native Responses carries its own `response.completed`/`response.incomplete`
terminal event; the host supplies chat protocol tails. Converted chat output
maps final usage and does not duplicate full done/terminal snapshots.

Only the separately mounted plugin artifact needs updating. No custom
CLIProxyAPI image and no Compose, updater, configuration, or `.env` changes are
required; official host auto-updates remain compatible.

## Model mapping

The plugin runs its own executor against `api.commandcode.ai`, so the host's
`openai-compatibility` alias table does not apply to its requests. commandcode
only accepts fully-qualified vendor names, and rejects a bare alias with
`Model "deepseek-flash" is not supported on this endpoint` — so the mapping has
to happen here.

Fields match the host's alias convention: `name` is what goes upstream,
`alias` is the relative client alias. Clients add the `commandcode/` prefix,
for example `commandcode/deepseek-flash`.

```yaml
    commandcode:
      models:
        - alias: deepseek-flash
          name: deepseek/deepseek-v4.1-flash
        - alias: glm-5.3-flash
          name: z-ai/glm-5.3-flash
          display_name: "GLM 5.3 Flash"     # optional label
```

When the vendor renames a model, edit this list — no code change. Omitting
`models` entirely uses the built-in defaults (those two entries).
An entry with no `name` still requires the `commandcode/` prefix and does not
rewrite the outbound model; use it only when the host supplies a valid upstream
name.

Routing strictly requires the `commandcode/` prefix and a configured model.
Aliases, upstream names and basename shorthand remain supported **within that
namespace**, with optional thinking suffixes. Default examples:

| Client model ID | Routed to this plugin? |
| --- | --- |
| `commandcode/deepseek-flash` | Yes |
| `commandcode/deepseek-v4.1-flash(high)` | Yes |
| `commandcode/deepseek/deepseek-v4.1-flash` | Yes |
| `commandcode/z-ai/glm-5.3-flash` | Yes |
| `deepseek-flash` / `deepseek-v4.1-flash` | No |
| `deepseek/deepseek-v4.1-flash` / `glm-5.3-flash` | No |
| `opencode-go/deepseek-v4.1-flash` | No |
| `commandcode/gpt-5` (not configured) | No |

Explicitly configured namespaced aliases also require the plugin prefix:
`alias: team/fast` is requested as `commandcode/team/fast`, not `team/fast`.
Whitespace and case normalization are unchanged. `RequestedModel` is
authoritative when present; the JSON body is consulted only when that field is
absent, so a conflicting body model cannot override the requested namespace.

## Install

```yaml
plugins:
  enabled: true
  configs:
    commandcode:
      enabled: true
      priority: 100
      api_keys:
        - key: user_YOUR_FIRST_KEY
          weight: 10
          proxy_url: http://127.0.0.1:18080   # optional per-key proxy
        - key: user_YOUR_SECOND_KEY
          weight: 5
          # disabled: true  # optional per-key kill switch (default false)
          # no proxy_url -> host HTTP client (host proxy policy + request-log)
```

Legacy single-key form (`api_key: user_...`) still works and equals a
one-member pool. Selection is weighted-random per request.

### Failover

Retryable — fails over to the next member:

- transport errors (connection reset, timeout, DNS)
- `401`, `402`, `429` and `5xx`
- any other `4xx`/`5xx` whose body carries a quota or billing signal:
  `insufficient_quota`, `insufficient_credits`, `insufficient credit`,
  `insufficient_balance`, `exceeded your current quota`, `quota
  exceeded`/`exhausted`/`depleted`/`reached`, `credit balance`, `balance too
  low`, `not enough credits`, `out of credits`, `no credits`, `no remaining
  quota`, `payment required`, `billing`. This is how a `403` that really means
  "this account is out of quota" still switches accounts.

Never retryable — validation and permission failures (`400`, `403` without a
quota signal, `404`, `422`, `3xx`) return the original upstream error
immediately, because another key cannot fix a malformed request.

Each round tries every enabled member once. At most **3 rounds**, waiting
**1 second** before round 2 and **2 seconds** before round 3, so two enabled
keys allow at most 6 attempts. Cancellation stops retries and backoff
immediately; if every attempt fails, the last error is returned.

Failover applies only before useful output: native lifecycle events are held
until that boundary, so an early failure can switch keys without leaking a
failed response ID or sequence. A stream that fails after output starts is
forwarded without replay, to avoid duplicate output or tool calls. Transport failures stay ambiguous — the upstream may
have processed the request before the connection broke, so a retry can
duplicate upstream work or billing. Members with `proxy_url`
(http/https/socks5) use a self-built transport — host request-log cannot
capture those outbound calls. Members with `disabled: true` are excluded
from selection without deleting them; disabling every defined member fails
closed (no silent fallback to the legacy key). Toggling takes effect on
plugin reconfigure/restart, no rebuild needed.

On the ModelRouter path the host passes a nil auth to the executor, so the
key **must** come from `plugins.configs.commandcode.api_keys` (or legacy
`api_key`). Then restart:

```bash
docker restart cli-proxy-api
docker logs cli-proxy-api | grep commandcode
# pluginhost: plugin registered plugin_id=commandcode plugin_name=CommandCode Provider
```

### Install from a registry URL

The plugin store installs from a `registry.json` source, not from a repository
URL. Add this fork's registry as an extra source:

```yaml
plugins:
  enabled: true
  store-sources:
    - "https://raw.githubusercontent.com/xspeed1989/cpa-plugin-commandcode/main/registry.json"
```

Then install `commandcode` from the management UI, or call
`POST /v0/management/plugin-store/commandcode/install?source=<source-id>`.
Entries without an `install` block resolve as `github-release`: tag
`v<version>` must carry `commandcode_<version>_<goos>_<goarch>.zip` plus
`checksums.txt`, which the tag-triggered workflow in
`.github/workflows/build.yml` publishes. To ship a change this way, bump the
version in `registry.json`, `plugin.go`, `cmd/commandcode/abi.go`, `build.sh`
and this README, then push tag `v<version>`.

Optional overrides:

```yaml
    commandcode:
      # Default: the two mappings shown under "Model mapping".
      models:
        - alias: my-alias
          name: vendor/model-name
      base_url: https://mirror.example.com/provider/v1    # default: https://api.commandcode.ai/provider/v1
```


## Build

Debian/glibc toolchain only (the runtime image is Debian; musl `.so` fails
to `dlopen`). Requires Go >= 1.26:

```bash
./build.sh
```

Runs `go vet`, `go test`, then `go build -buildmode=c-shared` for
`./cmd/commandcode`, emitting `commandcode-v<version>.so` into
`plugins/linux/amd64/`. The build injects the same version into the plugin's ABI
registration metadata; the default artifact and metadata version is `0.3.7`.

## Test

```bash
go vet ./... && go test ./...
```

Covers dual input/output format declarations and independent protocol
selection, native request/response/event preservation (encrypted reasoning,
annotations, custom tools, extension fields and integer precision), native SSE
framing and private-prelude failover, background response statuses and
format-correct token estimates. Also covers upstream `/responses` requests
(model mapping, input history, function tools/results, multimodal content,
structured output and stream flags),
Responses-to-chat text/reasoning/tool/usage conversion, parallel tool indices,
done-snapshot deduplication, incomplete results, in-band failure classification
and truncated-stream boundaries. Also covers the legacy reasoning backfill
(details-array priority, plain-string fallback, existing-`reasoning_content`
passthrough), SSE buffering and
normalization (split reads, stacked `data:` collapse, malformed/control-line
filtering, `[DONE]` swallowing), exact-path framing policy, failover
classification (which status codes and quota signals switch accounts, which
fail fast), retry rounds and backoff, cancellation, stream-error boundaries,
error/cancellation propagation, alias→upstream model mapping, and router
ownership, including required `commandcode/` prefixes, bare-name rejection,
foreign-provider namespace isolation and conflicting request/body models.

## Release

### v0.3.7

- Require the `commandcode/` prefix for every plugin-owned model. Bare aliases,
  bare shorthand and unprefixed vendor names no longer invoke this plugin.
- Treat `RequestedModel` as authoritative; consult the JSON body only when it
  is absent, preventing fallback from bypassing namespace restrictions.
- Preserve prefixed model mappings and thinking suffixes; expand regression
  tests and document the client-side prefix requirement.

### v0.3.6

- Fix the CommandCode model router claiming OpenCode Go and other providers'
  prefixed DeepSeek/GLM models by their shared basename.
- Preserve existing aliases, upstream model names, `commandcode/` variants,
  thinking suffixes and explicitly configured namespaced aliases.
- Add regression coverage for routing from both `RequestedModel` and JSON body.

### Publishing

1. `./build.sh` (or `PLUGIN_VERSION=x.y.z ./build.sh`).
2. Package `commandcode_<version>_<goos>_<goarch>.zip` files with the
   library at the zip root (Linux: `commandcode.so`, Darwin:
   `commandcode.dylib`, Windows: `commandcode.dll`).
3. Generate `checksums.txt` (sha256 of the zips).
4. `gh release create v<version> *.zip checksums.txt`

## License

MIT
