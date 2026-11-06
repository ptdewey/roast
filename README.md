# Roast

Roast is an adversarial, cross-model code reviewer. A finder model proposes
defects in a Git diff, each with a code quote and a concrete fix; an
independent verifier model then re-reads the cited code and confirms or refutes
every claim before it reaches the report. The two roles can use different
providers, endpoints, and wire protocols.

Roast has no telemetry. Models see only what Roast sends them, and in
interactive mode they can navigate the local checkout only through bounded,
read-only file, grep, and listing tools.

## Install

With Homebrew:

```sh
brew install --cask bluesky-social/tap/roast
```

Homebrew 7 and later only load casks from third-party taps you trust, though
naming the cask in full (as above) counts. For a plain `brew upgrade` to update Roast,
also run `brew trust --tap bluesky-social/tap`.

Or download an archive for macOS, Linux, or Windows (amd64 and arm64) from
[GitHub Releases](https://github.com/bluesky-social/roast/releases) and verify
it against the published SHA-256 checksums. Or, with Go 1.27.3 or newer:

```sh
go install github.com/bluesky-social/roast/cmd/roast@latest
```

To build from a checkout, run `go build ./cmd/roast`. `roast --version` prints
the version, commit, and build date.

Builds that report a version other than `dev` run a cached, informational check
for newer stable releases. It never blocks a command or changes its exit
status, and it fails open; set `ROAST_NO_UPDATE_CHECK=1` to turn it off.

Upgrading from a pre-1.1 configuration? See
[Upgrading from pre-1.1 configurations](docs/upgrading.md).

### Agent Skill

```sh
roast skill install
roast skill status
```

This installs the bundled skill to `~/.agents/skills/roast`, and also to
`$CLAUDE_CONFIG_DIR/skills/roast` (default `~/.claude/skills/roast`) when Claude
Code is detected and to `$XDG_CONFIG_HOME/polytoken/skills/roast` (default
`~/.config/polytoken/skills/roast`) when Polytoken is detected. `--dir` limits
either command to one parent skills directory.

## Quick start

```sh
export OPENAI_API_KEY='...'
export ROAST_FINDER_MODEL='your-finder-model'
export ROAST_VERIFIER_MODEL='your-verifier-model'
roast doctor
roast review --task 'add retry handling'
```

Both roles default to the OpenAI Responses protocol at
`https://api.openai.com/v1`.

## Codex subscription (older OAuth flow)

For the older Codex-style subscription login used by tools such as Albedo and Pi,
select the separate `codex` provider:

```sh
roast auth login --provider codex
roast auth status --provider codex
roast auth models --provider codex
```

This uses the legacy Codex public OAuth client and its minimal identity/offline
scopes, not dynamic third-party app registration or the newer **Use ChatGPT plan**
permission. Requests go directly to the Codex Responses backend; no Codex CLI
installation or `codex exec` wrapper is needed. Roast keeps its own prompts,
read-only tools, finder/verifier loop, and structured results.

Select model slugs from `auth models --provider codex` in your user configuration:

```yaml
version: 1
inference_profiles:
  codex:
    auth: codex
    account: default
    protocol: responses
    model: your-codex-model-slug
roles:
  finder:
    profile: codex
  verifier:
    profile: codex
```

Then run `roast doctor` or `roast review` normally. Use separate profiles/model
slugs for the finder and verifier if desired. `auth: codex` selects only
`https://chatgpt.com/backend-api/codex`; it cannot be combined with an API key or
redirected to `api.openai.com`, a gateway, or another protocol. Ambient OpenAI
API keys and base URLs are ignored. Conflicting role overrides fail closed.

- Pass `--provider codex` to **all** Codex account commands, including
  `roast auth logout --provider codex`. Without `--provider`, auth commands retain
  their existing `chatgpt` (newer app plan-sharing) behavior. There is no automatic
  fallback between these providers.
- The callback URI is fixed at `http://localhost:1455/auth/callback`, with a
  listener bound to IPv4 loopback only. Port 1455 must be available; finish other
  Codex/Pi login attempts first. `--no-browser` prints the URL for manual opening
  and does not change the callback URI or make a remote listener locally reachable.
- Accounts, rotating refresh tokens, and workspace identifiers are stored under
  `roast/auth/codex` in your platform user configuration directory, separately from
  `roast/auth/chatgpt`. Existing SIWC registrations and Albedo/Pi/Codex credential
  files are never imported, overwritten, or used implicitly. Sign in once for
  Roast; use `--account personal` with `account: personal` for a named account.
- Refreshes are serialized across processes. Access tokens and workspace IDs are
  obtained together from the same account; returning login or refresh cannot
  silently change the stored identity/workspace. Use another account label for
  a different identity or workspace.
- Logout revokes the session if the trusted discovery document advertises a
  revocation endpoint, then clears local tokens. A revocation failure retains the
  credentials for retry. If no revocation endpoint is advertised, Codex logout
  clears local tokens only; it does not claim to revoke a remote session.
- This is the legacy Codex client protocol, not OpenAI's newer third-party
  plan-sharing contract. OpenAI can change it; eligible models, subscription
  limits, and any Codex-specific workspace policies still apply. It does not
  bypass a workspace's Codex access policy. Model discovery advertises client
  protocol version `0.162.1`, so a future model requiring a newer version may
  require a Roast update.
- Like the newer subscription route, Codex does not accept a server-enforced
  `max_output_tokens` cap. Roast retains local reservations, usage accounting,
  turn limits, and deadlines; a response may exceed its reservation.

## ChatGPT subscription (app plan sharing)

Eligible ChatGPT Plus and Pro users can authorize Roast to use their subscription
through OpenAI's [Sign in with ChatGPT flow](https://developers.openai.com/siwc/token-sharing-open-source).
This is a direct Responses API integration: Roast still controls the prompts,
read-only tools, finder/verifier conversations, and structured findings. It does
not run Codex or read Codex's credentials. This preview is intended for locally
run open-source tools; eligibility, model availability, and usage limits are
controlled by OpenAI.

First sign in and list the models available to your account:

```sh
roast auth login
roast auth status
roast auth models
```

`auth login` opens your system browser and listens on `127.0.0.1` for the OAuth
callback. Use `--no-browser` to open the printed URL yourself, and `--timeout 10m`
to change the default five-minute sign-in timeout. The browser must be able to
reach the loopback listener on the machine running Roast. `auth status --json`
and `auth models --json` provide machine-readable, non-secret output.

In your user-owned `roast/config.yaml`, select a model slug from `auth models`:

```yaml
version: 1
inference_profiles:
  subscription:
    auth: chatgpt
    account: default
    protocol: responses
    model: your-account-model-slug
    max_output_tokens: 32768
roles:
  finder:
    profile: subscription
  verifier:
    profile: subscription
```

Then run `roast doctor` and `roast review` normally. Finder and verifier may use
separate profiles, models, and auth modes; an API-key-backed Claude verifier can
be paired with a ChatGPT-backed finder, for example. This does **not** enable
Claude subscription authentication.

- `auth` defaults to the existing API-key behavior (`api_key`). `chatgpt` is
  explicit and supports only `protocol: responses` at
  `https://api.openai.com/v1`. It cannot be combined with `api_key` or
  `api_key_env`. Ambient `OPENAI_API_KEY` and `OPENAI_BASE_URL` are ignored for
  subscription profiles. Incompatible role keys, endpoint/protocol environment
  overrides, and endpoint/protocol flags are rejected rather than falling back
  to another credential or sending tokens to a gateway.
- `account` is a local label, defaulting to `default`. Use
  `roast auth login --account personal` and `account: personal` for a separate
  registration. Labels contain 1–64 letters, digits, hyphens, or underscores,
  starting with a letter or digit. Returning login preserves the registered
  account and workspace; use another label to add a different account.
- OAuth credentials are stored separately from configuration and run artifacts,
  under `roast/auth/chatgpt` in the platform's user configuration directory.
  Treat this directory like a password: it contains access, refresh, and ID
  tokens. Do not commit or share it. Tokens refresh automatically, with account
  locking to serialize refreshes across concurrent reviews.
- `roast auth logout [--account personal]` revokes the renewable session before
  clearing its local tokens. If revocation fails, credentials are retained so
  logout can be retried; you can also disconnect Roast in ChatGPT Settings.
  The local registration and stable host identifier are retained for later
  sign-in.
- Subscription requests require streaming and `store: false`; Roast replays its
  conversation history itself. OpenAI's current subscription route does **not**
  accept `max_output_tokens`, so that setting is a local budget reservation,
  not a server-enforced output cap. Token accounting, turn limits, and deadlines
  still apply, but a single response can exceed its reservation. `roast doctor`
  reports this limitation. ChatGPT plan/app usage limits apply independently;
  Plus usage is shared across participating apps. See ChatGPT Settings → Usage
  to manage app limits and access.

## Configure

### Environment variables

Each role can use its own endpoint, credential, and protocol:

```sh
export ROAST_FINDER_BASE_URL='https://finder.example/v1'
export ROAST_FINDER_API_KEY='...'
export ROAST_FINDER_MODEL='finder-model'
export ROAST_FINDER_PROTOCOL='chat'

export ROAST_VERIFIER_BASE_URL='https://verifier.example/v1'
export ROAST_VERIFIER_API_KEY='...'
export ROAST_VERIFIER_MODEL='verifier-model'
export ROAST_VERIFIER_PROTOCOL='responses'
```

Protocols are `chat`, `responses`, and `anthropic` (native Messages, which
authenticates with `x-api-key` and defaults to `https://api.anthropic.com/v1`):

```sh
export ANTHROPIC_API_KEY='...'
export ROAST_FINDER_MODEL='claude-model-name'
export ROAST_FINDER_PROTOCOL='anthropic'
```

Any base URL works, so a role can point at a hosted provider, a gateway, or a
local server that implements the selected protocol.

### Config file

For named profiles, use `roast/config.yaml` in your user config directory (see
[Config file location](#config-file-location)) or pass `--config`:

```yaml
version: 1

inference_profiles:
  claude:
    endpoint: https://api.anthropic.com/v1
    model: claude-model-name
    protocol: anthropic
    api_key_env: ANTHROPIC_API_KEY
    max_output_tokens: 32768
  verifier:
    endpoint: https://verifier.example/v1
    model: verifier-model
    protocol: responses
    api_key_env: OPENAI_API_KEY
    max_output_tokens: 32768

roles:
  finder:
    profile: claude
  verifier:
    profile: verifier
```

A profile is a complete unit: model, `endpoint`, `protocol`, output-token limit,
and credential source. Roast never infers a protocol or credential from a model
name, and never reads configuration from the repository under review, so a
checkout cannot redirect inference or supply credentials.

`api_key_env` names an environment variable that is read only when the profile
is selected. A literal `api_key` also works, but environment references are
preferable. `version: 1` is required for new configs; Roast migrates its two
legacy unversioned formats in memory, and rejects unknown fields and
unsupported versions.

### Credentials and precedence

Command-line role flags override `ROAST_FINDER_*` / `ROAST_VERIFIER_*`
variables, which override profiles. A role's credential is the first of:

1. `ROAST_FINDER_API_KEY` or `ROAST_VERIFIER_API_KEY`;
2. the profile's `api_key_env` or `api_key`;
3. the provider's `OPENAI_API_KEY` or `ANTHROPIC_API_KEY`, but only when the
   role's endpoint is that provider's default or its `OPENAI_BASE_URL` /
   `ANTHROPIC_BASE_URL`.

Provider variables are fallbacks, never overrides: a provider `*_BASE_URL`
applies only to a profile with no `endpoint`, and the `OPENAI_*` variables
apply only to `chat` and `responses` while `ANTHROPIC_*` apply only to
`anthropic`. A profile with any other endpoint must name its credential, so a
key in your shell is never sent implicitly to a gateway or a configuration
template's endpoint. When a protocol override moves a role to another
provider's endpoint, the profile's credential does not follow it. Flags and
environment overrides follow the same rules, and `roast doctor` reports which
source each role used.

### Author routes

When the invoking agent passes `--author-model` (or `ROAST_AUTHOR_MODEL`), the
finder can switch to another profile by exact model name or model family, so a
model is not reviewing its own work. Here `primary` and `independent` are
profiles defined under `inference_profiles`:

```yaml
roles:
  finder:
    profile: primary
    author_routes:
      openai: independent
      exact-author-model: independent
```

`roles.finder.pin: true` keeps the configured finder anyway, with a warning.

### Other settings

| Key | Default | Effect |
| --- | --- | --- |
| `review.excludes` | none | Git pathspecs excluded from every review, in addition to `--exclude`. |
| `review.confidence_floor` | `0.6` | Findings below this finder confidence (0–1) are dropped before verification and listed under `dropped_findings`. `0` selects the default. |
| `review.quick_confidence_floor` | `0.8` | The same floor for `--quick` reviews, which skip verification. |
| `roles.finder.pin` | `false` | Ignore `author_routes` even when the author shares the finder's model family. |
| `prompts.finder_code`, `prompts.finder_prose`, `prompts.verifier` | built in | Absolute paths to prompt overrides. |

Runs record the profile name and resolved non-secret settings, never API keys.
The invocation argv is recorded, so keep secrets out of command-line values.

### Config file location

Without `--config`, Roast reads `roast/config.yaml` from the user configuration
directory:

| Platform | Default path |
| --- | --- |
| macOS | `~/Library/Application Support/roast/config.yaml` |
| Linux and other Unix | `$XDG_CONFIG_HOME/roast/config.yaml`, or `~/.config/roast/config.yaml` when `XDG_CONFIG_HOME` is unset |
| Windows | `%AppData%\roast\config.yaml` |

macOS and Windows ignore `XDG_CONFIG_HOME`; where it applies, it must be an
absolute path. A missing default file is not an error. `roast doctor` reports
the path it used.

### Configuration URLs

An organization can publish a secret-free configuration document, installed
with `roast config init --from <https-url>`. The document must declare
`version: 1`, may name credential environment variables, and must not contain
credentials or control characters. A template can still name a variable such
as `OPENAI_API_KEY` for its own endpoint, which sends that key there, so Roast
prints a summary of each selected profile's endpoint and credential variable,
and of any upload destination, when it installs a template.

When stdin and stderr are terminals and no `--config` is given, `roast review`
and `roast runs sync` also offer this prompt if the default config is missing
and the roles cannot be resolved from the environment or flags, or if the
config is unversioned or has Claude profiles using Chat Completions or
Responses. A [presigned upload broker](#presigned-upload-broker) can serve the
document.

## Use

Run Roast inside a Git repository:

```sh
roast review --task 'add retry handling'   # review the current changes
roast review --staged --max-severity HIGH  # staged changes; exit 1 on HIGH+
roast review --revision <rev>              # exactly one Git commit or jj revision
roast review --interactive                 # let the finder navigate the repo
roast review --quick                       # finder only, no verification
roast review --scope repo                  # survey the whole repository
roast runs                                 # list local runs
roast runs show <run-id>
roast runs checkout <run-id>
roast doctor
```

The default scope is committed changes since the best available main branch,
plus uncommitted and untracked changes. Untracked files that look like secrets
(`.env*`, `*.pem`, `*.key`, SSH keys such as `id_ed25519*`, and similar) are
skipped with a note on stderr. JSON goes to stdout and Markdown to stderr;
`--format` selects one.

`--revision <rev>` reviews only the selected commit against its first parent
(or the empty tree for an initial commit), without including other checkout,
index, or untracked changes. In a jj workspace, `<rev>` is a jj revset that must
resolve to exactly one revision; elsewhere it is a Git commit ref. Roast prints
the resolved commit ID and pins all review context to that revision. Invalid or
ambiguous targets fail rather than falling back to another revision. This
requires a Git repository, or a colocated Git-backed jj workspace with `jj` on
PATH. Non-colocated jj workspaces are rejected explicitly. `--revision @`
snapshots and reviews the current jj change, including its latest edits. Clear
custom Git routing variables (such as `GIT_INDEX_FILE`) before using `--revision`;
these overrides are rejected to protect the live repository during isolation.
`--revision` cannot be combined with `--base`, `--staged`,
`--uncommitted-only`, or `--scope repo`. Without it, scope selection is unchanged.

`--interactive` gives the finder several turns with the navigation tools. Small
diffs (under 80 lines and 4 files) get a single structured call instead, and
Roast says so; `ROAST_INTERACTIVE_MIN_DIFF_LINES` and
`ROAST_INTERACTIVE_MIN_FILES` change those thresholds, and `0` disables them.
`--quick` turns navigation off. `--scope repo` always navigates and rejects
`--quick`.

The finder and verifier must submit schema-validated results through a tool
call. A role that never does gets bounded reminders within its limits; if it
still fails, the run is reported incomplete, never clean.

`roast doctor` validates profiles and credentials, lists each provider's
models, makes one small tool-call request per configured model, and reports
Git, gate, Agent Skill, upload, and release status. `--json` gives
machine-readable output.

### What is sent where

Source code, diffs, prompts, tool schemas, and tool results go to the inference
endpoints you configure, and run bundles go to your upload destination if you
enable uploads. Review data goes nowhere else; Roast's only other network calls
are the release check and configuration URLs you ask it to fetch.
Review your providers' data-handling terms before using Roast on sensitive
code. The [security model](docs/security-model.md) covers trust boundaries and
artifacts in full.

Each request carries an `X-Agent-Session-Id` that is stable across one model
conversation's tool turns and retries and differs between conversations. It
identifies a conversation, not a person or a whole run.

## Resource limits

Defaults are generous enough for slow local models, and every tunable limit is
yours to change:

| Control | Default | Override |
| --- | ---: | --- |
| Output tokens per model call | 32,768 | Profile `max_output_tokens`, `ROAST_{FINDER,VERIFIER}_MAX_OUTPUT_TOKENS`, or `--{finder,verifier}-max-output-tokens` |
| Timeout per inference request | 10 minutes | `--request-timeout` / `ROAST_REQUEST_TIMEOUT`; `0` disables |
| Interactive finder work deadline | 10 minutes | `--deadline` / `ROAST_DEADLINE`; `0` disables |
| Repository-survey work deadline | 25 minutes | `--deadline` / `ROAST_DEADLINE`; `0` disables |
| Provider/structured-output attempts | 4 | `--max-attempts` / `ROAST_MAX_ATTEMPTS` |
| Shared finder/verifier token budget | 3,000,000 | `--max-tokens-total` / `ROAST_MAX_TOTAL_TOKENS`; `0` disables |
| Findings sent to the verifier | 25 | `--max-verified` / `ROAST_MAX_VERIFIED`; `0` disables |
| Diff bytes, normal / quick (also caps each file's section) | 1,000,000 / 200,000 | `ROAST_MAX_DIFF_BYTES` |
| File-context bytes, normal / quick | 400,000 / 100,000 | `ROAST_MAX_FILE_CTX_BYTES` |
| Initial finder prompt bytes | 8,000,000 | `ROAST_MAX_PROMPT_BYTES` |

Interactive runs default to 16 finder turns and surveys to 40 (`--max-turns`).
Past roughly 3 MB of conversation, Roast tells the finder to stop exploring and
submit; a work deadline leaves a separate 90-second window for that final
submission.

Fixed caps protect memory and artifacts: 16 MiB per captured provider response;
64 KiB per `git_diff` window and per `read_file`, `grep_repo`, or `list_files`
result; `read_file` lines truncated at 32 KiB, 2,000 lines per window;
`grep_repo` lines truncated at 2 KiB; at most 500 listed paths and 200 matching
lines.

Invalid or negative overrides are usage errors, not silent fallbacks. Resolved
limits are recorded with each run.

## Exit codes

| Code | Meaning | `error_category` |
| ---: | --- | --- |
| 0 | Clean completion | `none` |
| 1 | A finding met the `--max-severity` threshold | `findings` |
| 2 | Invalid invocation or configuration | `usage` |
| 3 | Inference authentication failed | `auth` |
| 4 | Operational failure or incomplete result | `endpoint`, `resource_limit`, or `partial` |
| 5 | Cancelled or terminated | `cancelled` |

`--error-format json` writes diagnostics to stderr as JSON without changing
review output:

```json
{"error_category":"endpoint","message":"...","exit_code":4}
```

## Run records

Every non-empty review is recorded under `$XDG_DATA_HOME/roast` (default
`~/.local/share/roast`; override with `ROAST_RECORD_DIR`). A run holds the
secret-free role configuration, argv, exact prompts and reviewed input, raw
provider response bodies (streamed SSE kept byte-for-byte, including partial
streams from failed attempts), usage, results, a Git snapshot, and a
deterministic `bundle.tar.zst` whose manifest lists every artifact's size and
SHA-256. Requests, HTTP headers, and credentials are never recorded. Run
directories are kept until you delete them.

The navigation tools read the snapshot, not the live checkout: the captured
index for `--staged`, otherwise the captured worktree including non-ignored
untracked files. Editing files mid-review changes nothing the models see.

Snapshots are Git refs under `refs/roast/snapshots/`. `roast runs checkout
<run-id> [dir]` restores one as a detached linked worktree, with the reviewed
contents on disk and the reviewed index staged, so `git status` and `git diff`
show exactly what was reviewed. `roast runs prune` removes snapshot refs older
than 30 days (`--dry-run` to preview) but never run directories.
`ROAST_SNAPSHOT=0` skips snapshot capture, so such a run cannot be checked out.

## Run uploads

Uploads are off by default. When enabled, Roast uploads each completed review's
bundle on a best-effort basis; failures never change the review result and stay
pending for `roast runs sync`. `roast runs status` shows upload state. Runs
recorded before you opted in are uploaded only with
`roast runs sync --include-existing`. Running `roast runs sync` is itself an
explicit upload request, so it uploads even while automatic upload is disabled.

To upload directly to an S3-compatible store (AWS S3, MinIO, Cloudflare R2, and
others):

```yaml
upload:
  enabled: true
  backend: s3
  endpoint: https://objects.example.com
  bucket: roast-runs
  prefix: team-name
  region: us-east-1
  access_key_env: ROAST_UPLOAD_ACCESS_KEY
  secret_key_env: ROAST_UPLOAD_SECRET_KEY
  # session_token_env: ROAST_UPLOAD_SESSION_TOKEN
  path_style: false
```

Credentials come only from the named environment variables, never from AWS
profiles, SSO, or instance metadata. Objects are content-addressed at
`<prefix>/sha256/<bundle-digest>.tar.zst`. Endpoints must use HTTPS unless on
loopback; `allow_insecure_http: true` permits plaintext on a trusted local
network and must never be used across an untrusted one.

Organizations that don't hand out storage credentials can run a
[presigned upload broker](#presigned-upload-broker) instead:

```yaml
upload:
  enabled: true
  backend: presigned
  endpoint: https://roast-uploads.example.com
  token_env: ROAST_UPLOAD_TOKEN   # or a literal upload.token in your own config
```

The bearer token goes only to the broker. The broker returns a short-lived,
content-bound PUT URL; Roast uploads the bundle straight to storage without the
token, then asks the broker to verify it.

## Presigned upload broker

`roast-broker` is a small stateless service, built by the root `Dockerfile`. It
relies on a trusted reverse proxy for authentication: on `/api/v1/roast/runs`
and its completion endpoint, the proxy must strip any incoming principal
header, validate `Authorization: Bearer`, and inject the verified principal.
`/healthz` and the optional `/config/v1` are unauthenticated so new clients can
fetch configuration. Never expose the container directly; every route shares
one listener, and the API trusts the principal header.

Required environment:

```text
ROAST_BROKER_PRINCIPAL_HEADER=X-Forwarded-Email
ROAST_BROKER_SIGNING_KEY=<at least 32 random bytes>
ROAST_BROKER_S3_ENDPOINT=https://objects.example.com
ROAST_BROKER_S3_BUCKET=roast-runs
```

Storage credentials come from `ROAST_BROKER_S3_ACCESS_KEY`,
`ROAST_BROKER_S3_SECRET_KEY`, and optional `ROAST_BROKER_S3_SESSION_TOKEN`, or
from the MinIO client's IAM/workload-identity chain when those are unset. Also
available: `ROAST_BROKER_S3_REGION`, `ROAST_BROKER_S3_PREFIX`,
`ROAST_BROKER_S3_PATH_STYLE`, `ROAST_BROKER_MAX_BUNDLE_BYTES`, and
`ROAST_BROKER_LISTEN_ADDRESS` (default `:8080`). Set
`ROAST_BROKER_CLIENT_CONFIG_FILE` to a validated, secret-free client config to
serve it at `/config/v1` as a configuration URL; startup fails if it is
unversioned, invalid, or contains credentials.

The protocol has three steps:

1. `POST /api/v1/roast/runs` declares the run ID, bundle size, and SHA-256, and
   returns a presigned PUT and an opaque completion token.
2. The client PUTs the bundle with exactly the returned headers and no broker
   credential.
3. `POST /api/v1/roast/runs/{run_id}/complete` presents the token; the broker
   checks ownership and the stored object's size and checksum.

Each grant gets a unique object key, so the broker needs no database. Configure
bucket lifecycle expiration to clean up abandoned uploads.

## Pre-commit gate

```sh
roast gate enable
roast gate status
roast gate disable
```

The gate runs a `--quick` review of the staged content on each commit and
blocks only when a finding meets the severity threshold. It fails open: a
missing binary, failed inference, empty output, or timeout prints a warning and
allows the commit.

The first interactive review in a repository asks once whether to enable it and
records the answer in local Git config; non-interactive runs never install a
hook without that consent. `ROAST_NO_HOOK=1` skips the question. With a shared
`core.hooksPath`, each repository still needs its own opt-in, and disabling one
leaves the dispatcher in place for the others.

| Variable | Effect |
| --- | --- |
| `ROAST_SKIP` | Any non-empty value (conventionally `1`) skips one commit. |
| `ROAST_BIN` | The `roast` binary to run instead of the one on `PATH`. |
| `ROAST_GATE_TIMEOUT` | Seconds before an unfinished review is stopped and the commit allowed. Default `180`; `0` disables; a non-numeric value uses the default. |
| `ROAST_GATE_SEVERITY` | Lowest blocking severity: `INFO`, `LOW`, `MEDIUM`, `HIGH` (default), or `CRITICAL`. An invalid value fails the review, so the commit is allowed. |

## Contributing

Pull requests are squash-merged, so each title becomes a commit on `main` and a
release-notes entry. Titles need a conventional `type:` prefix (checked in CI):
`feat:` releases a minor version, `feat!:` or a `BREAKING CHANGE:` footer a
major version, and any other type, such as `fix:` or `docs:`, a patch. See
[CONTRIBUTING.md](CONTRIBUTING.md) for development checks and release steps,
and [SECURITY.md](SECURITY.md) to report a vulnerability.

## License

Dual-licensed under [MIT](LICENSE-MIT.txt) or
[Apache 2.0](LICENSE-APACHE.txt), at your option. Dependencies keep their own
licenses; CI checks the linked dependency graph against the allowed set.
