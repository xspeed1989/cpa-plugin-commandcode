# cpa-plugin-commandcode

CLIProxyAPI native provider plugin for [commandcode.ai](https://api.commandcode.ai) (`api.commandcode.ai/provider/v1`).

CommandCode's OpenAI-compatible endpoint returns thinking text under `reasoning`
(string) and `reasoning_details[].text`, but never the standard
`reasoning_content` field. CLIProxyAPI's built-in openai→claude translator only
reads `reasoning_content`, so the field must be backfilled before translation.
For streaming `/v1/messages`, that translator also requires each OpenAI chunk
to arrive with an SSE `data: ` prefix; bare executor JSON is discarded.

This plugin implements a full provider (`ModelProvider + ModelRouter +
Executor + Request/Response translators`) that forwards chat-completions to
commandcode, normalizes responses into standard OpenAI shape, and applies the
route-specific transport framing expected by the host. This restores streaming
thinking, text, and usage on `/v1/messages` without modifying CLIProxyAPI.

## Capabilities

- `model_provider` — advertises the configured models under the
  `commandcode/` namespace (so they never collide with the native
  openai-compatibility channel; the ABI has no live `/v1/models` discovery).
- `model_router` — hijacks the configured client aliases (`deepseek-flash`,
  `deepseek-vision`, `glm-5.3-flash` by default) to this executor.
- `executor` — POSTs to `/chat/completions` through the host HTTP client
  (proxy policy + request-log preserved); backfills `reasoning_content` on
  every non-streaming response and every SSE data line, then applies the
  endpoint-specific stream framing described below.
- `request_translator` / `response_translator` — the same reasoning backfill
  for translated edges.

## Streaming compatibility

The host rewrites `ExecutorRequest.Format` and `SourceFormat` to the executor's
OpenAI format, so they cannot identify the original client endpoint. The
executor instead reads the host's public `request_path` metadata and prefixes a
normalized chunk with exactly one `data: ` only when its value is exactly
`/v1/messages`.

`/v1/chat/completions` and `/v1/responses` remain bare: the Chat Completions
handler adds HTTP SSE framing itself, while the Responses translation accepts
bare executor chunks. Missing, non-string, unknown, or near-match paths also
remain bare (fail closed), preventing `data: data: {...}` output. The plugin
strips stacked upstream prefixes before applying this policy and leaves the
host responsible for terminal events and `[DONE]`.

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
`alias` is what clients send.

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
An entry with no `name` claims the alias but forwards it verbatim, which is
only correct for aliases the host resolves itself.

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

Failover applies only before the first normalized output chunk: a stream that
fails after output starts is forwarded without replay, to avoid duplicate
output or tool calls. Transport failures stay ambiguous — the upstream may
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
      # Default: the three mappings shown under "Model mapping".
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
registration metadata; the default artifact and metadata version is `0.3.4`.

## Test

```bash
go vet ./... && go test ./...
```

Covers the reasoning backfill (details-array priority, plain-string
fallback, existing-`reasoning_content` passthrough), SSE buffering and
normalization (split reads, stacked `data:` collapse, malformed/control-line
filtering, `[DONE]` swallowing), exact-path framing policy, failover
classification (which status codes and quota signals switch accounts, which
fail fast), retry rounds and backoff, cancellation, stream-error boundaries,
error/cancellation propagation, alias→upstream model mapping, and router
ownership.

## Release

1. `./build.sh` (or `PLUGIN_VERSION=x.y.z ./build.sh`).
2. Package `commandcode_<version>_<goos>_<goarch>.zip` files with the
   library at the zip root (Linux: `commandcode.so`, Darwin:
   `commandcode.dylib`, Windows: `commandcode.dll`).
3. Generate `checksums.txt` (sha256 of the zips).
4. `gh release create v<version> *.zip checksums.txt`

## License

MIT
