<p align="center">
  <img src="docs/assets/readme-cover.svg" alt="FutureSearch Gateway — one gateway for research, accounts, and control" width="100%">
</p>

<h1 align="center">FutureSearch Gateway</h1>

<p align="center">
  Claude, GPT, Gemini — one gateway, with research and account management built in.
</p>

<p align="center">
  <a href="README.md">中文</a> · <strong>English</strong>
</p>

<p align="center">
  <a href="https://go.dev/dl/"><img src="https://img.shields.io/badge/Go-1.24%2B-28764C?style=flat-square&logo=go&logoColor=white" alt="Go 1.24 or newer"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-MIT-28764C?style=flat-square" alt="MIT license"></a>
  <a href="https://github.com/hey345437-boop/futuresearch-gateway/actions/workflows/ci.yml"><img src="https://github.com/hey345437-boop/futuresearch-gateway/actions/workflows/ci.yml/badge.svg" alt="CI status"></a>
  <a href="https://github.com/hey345437-boop/futuresearch-gateway/releases"><img src="https://img.shields.io/github/v/release/hey345437-boop/futuresearch-gateway?style=flat-square&color=28764C" alt="Latest release"></a>
</p>

<p align="center">
  <a href="#quick-start">Quick start</a> · <a href="#panel">Panel</a> · <a href="#connect-clients">Connect clients</a> · <a href="#models">Models</a> · <a href="#configuration">Configuration</a> · <a href="#disclaimer">Disclaimer</a>
</p>

---

Bring **Claude, GPT, Gemini, and more** into the clients you already use. FutureSearch Gateway exposes the **complete 143-entry upstream model catalog** through one OpenAI-compatible connection, with research, accounts, balances, and prompts in one workspace. Model access follows your FutureSearch account permissions.

**Your advanced model lineup, together. One binary. One configuration file. One data directory.**

## Features

| Feature | What it does |
| :--- | :--- |
| **One connection for your clients** | OpenAI-compatible `/v1/chat/completions` with streaming and non-streaming responses, plus `/v1/models`. |
| **Account management** | Add keys, refresh balances, enable or disable accounts, and manage cooldowns. Reuses a healthy account, then favors higher available balances. |
| **Claude, GPT, Gemini, and more** | The complete 143-entry upstream model catalog, 35 family shortcuts, and 7 research presets — ready to browse and select. |
| **One workspace to stay in control** | Connection setup, accounts, tenant keys and quotas, prompts, usage, previews, and settings in the built-in panel. |
| **MCP tools** | Cloud research tools and an optional, separately configured set of local file tools. |
| **Local preview** | Serve generated HTML or other files from a configured directory at `/preview/`. |

## Quick start

### Docker

```bash
mkdir -p fsgw-data

docker run -d --name fsgw \
  --user "$(id -u):$(id -g)" \
  -p 127.0.0.1:7868:7868 \
  -v "$PWD/fsgw-data:/data" \
  -e FSGW_LISTEN_HOST=0.0.0.0 \
  -e FSGW_API_KEY=sk-your-client-key \
  -e FSGW_ADMIN_PASSWORD=your-panel-password \
  ghcr.io/hey345437-boop/futuresearch-gateway:latest
```

Open **[http://127.0.0.1:7868/](http://127.0.0.1:7868/)**, sign in with your panel password, and add your FutureSearch API key in **Accounts**. The published image supports Linux `amd64` and `arm64`.

`FSGW_API_KEY` is the key you give your clients. Your **FutureSearch account key** belongs in the panel; your **admin password** is for signing in.

### Download a release

Get the matching binary from **[Releases](https://github.com/hey345437-boop/futuresearch-gateway/releases/latest)**. Builds are available for macOS, Linux, and Windows, on `amd64` and `arm64`.

For example, after downloading the Apple Silicon build:

```bash
chmod +x fsgw-darwin-arm64
./fsgw-darwin-arm64
```

The default address is `127.0.0.1:7868`. The first run creates `config.json` and `data/` in the current directory.

### Build from source

Requires **Go 1.24 or newer**. The project uses the Go standard library, with no third-party Go dependencies.

```bash
git clone https://github.com/hey345437-boop/futuresearch-gateway.git
cd futuresearch-gateway
go build -o fsgw ./cmd/gateway
./fsgw
```

## Panel

![FutureSearch Gateway connection panel](docs/assets/panel-preview.jpg)

| Page | Main controls |
| :--- | :--- |
| **Overview / Connection** | View service status, add an account, and copy client configuration. |
| **Accounts / Tenants** | Refresh balances, manage account availability, and issue client keys with quotas. |
| **Models / Settings** | Browse model names, set global or per-model prompts, and edit service configuration. |
| **Preview** | Browse files in the configured local preview directory. |

The connection page can write DSH configuration on the **machine running the gateway**, with a backup of `settings.yaml`. In Docker, this refers to the container's filesystem; use the manual configuration below for DSH on your host.

## Connect clients

Use `http://127.0.0.1:7868/v1` as the OpenAI base URL and your **gateway client key** as the API key.

### DSH / pi-ai

Add the provider under `llm-pi-ai.providers` in `~/.dsh/settings.yaml`:

```yaml
llm-pi-ai:
  providers:
    futuresearch:
      displayName: FutureSearch Gateway
      apiKeyEnv: FSGW_KEY
      api: openai-completions
      baseURL: http://127.0.0.1:7868/v1
      timeoutMs: 1920000
      streamIdleTimeoutMs: 300000
      retryPolicy:
        mode: normal
        maxRetries: 0
      models:
        - id: agent-medium
          name: Agent Research
          reasoningEfforts: {low: low, medium: medium, high: high}
        - id: claude-fable-5
          name: Claude Fable-5
          reasoningEfforts: {low: low, medium: medium, high: high, max: max}
```

Set the credential in `~/.dsh/.credentials.yaml`, merging it into the existing `refs` section:

```yaml
version: 1
refs:
  FSGW_KEY: sk-your-client-key
```

`FSGW_KEY` contains the gateway key, not a FutureSearch account key. The longer timeouts allow research tasks to finish; `maxRetries: 0` avoids client-side resubmission of a paid task.

### curl

```bash
curl http://127.0.0.1:7868/v1/chat/completions \
  -H 'Authorization: Bearer sk-your-client-key' \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "agent-medium",
    "stream": false,
    "messages": [{"role": "user", "content": "Compare the main approaches to long-duration energy storage, with sources."}]
  }'
```

Set `"stream": true` to receive progress and the final answer through SSE.

### MCP

For clients that support Streamable HTTP MCP, use:

```json
{
  "mcpServers": {
    "futuresearch": { "url": "http://127.0.0.1:7868/mcp" }
  }
}
```

The research server provides `research(question, effort?, model?)`, `forecast(question)`, and `models()`. When local tools are enabled and configured, add a separate server pointing to `http://127.0.0.1:7868/mcp/local`.

For a client that launches an MCP process over stdio:

```bash
./fsgw --mcp-stdio --config /absolute/path/to/config.json
```

Stdio mode exposes the **research tools only** and uses the accounts in the configured data directory. HTTP MCP endpoints and `/preview/` currently do not use the OpenAI API or panel authentication, and MCP requests do not pass through tenant quotas. The examples above bind access to your local machine.

## Models

The catalog contains **143 complete upstream model entries**, grouped into **35 families**, alongside **7 research presets**. Family shortcuts select among those same upstream entries; these counts do not describe 185 different underlying models.

| Form | Example | Behavior |
| :--- | :--- | :--- |
| **Research preset** | `agent-low`, `agent-medium`, `agent-high` | FutureSearch selects the model and research depth. |
| **Multi-agent / forecast** | `multi-agent-medium`, `forecast` | Parallel research, or a probability forecast for a yes/no question. |
| **Family + effort** | `claude-fable-5` with `reasoning_effort: "max"` | Select a model family and a supported tier separately. |
| **Complete model name** | `claude-fable-5-max` | Put the tier in the name; useful when another gateway drops `reasoning_effort`. |

Explicit model selections use a single upstream call with `iteration_budget: 0`; use research presets for iterative research. Available tiers vary by family. The catalog is bundled with the gateway, and actual availability depends on the upstream service and your account.

## Advanced notes

<details>
<summary><strong>Task progress, streaming, and conversation history</strong></summary>

The upstream returns a task ID, then status and result endpoints are polled until completion. Progress is emitted as `reasoning_content` so clients can show activity while waiting. Answer chunks are sent after the task result arrives; this is not upstream token-by-token generation.

Successful streams end with `[DONE]`. Failed streams emit an error frame without a success marker. Each turn starts a fresh task, with conversation history folded into a truncated text preamble. Image input and model tool calling are not supported.

</details>

<details>
<summary><strong>Prompts, usage estimates, and tenant quotas</strong></summary>

The upstream API has no `system` field. Configured prompts are prepended to the task text. `*` is the global fallback, an exact model name overrides it, and an empty string disables injection for that model:

```json
{
  "prompts": {
    "*": "Give the conclusion first, then cite your sources.",
    "agent-high": ""
  }
}
```

Token usage is estimated from character counts. Tenant quotas use estimated per-request deductions, including failed calls; they are not the platform's actual billing ledger. The root gateway key bypasses tenant quotas.

</details>

<details>
<summary><strong>Optional local file tools and preview</strong></summary>

Local tools are disabled by default. Set `localfs.enabled` and a `localfs.root` directory to expose `list_dir`, `read_file`, and `search`. `allow_write` adds `write_file` and `mkdir`; `allow_exec` adds `run_command`. Paths stay inside the configured root, including resolved symlinks. Commands receive an argument array without an implicit shell.

Preview serves files from `preview.dir` at `preview.path`. On Windows, command execution requires an executable; shell scripts or `.cmd` wrappers need an explicit interpreter in the argument list.

A recorded end-to-end run (a tool-capable model driving this gateway's `/mcp/local`):

```
1. model calls list_dir(dir='fsgw')                 -> gateway returns the tree
2. model calls mkdir(path='fsgw/preview')           -> created
3. model calls write_file(path=..., content=...)    -> wrote 706 bytes
4. model reports done

disk:   -rw-r--r-- 706 B  <root>/fsgw/preview/hello-mcp.html
browser: GET /preview/hello-mcp.html -> 200
```

With a research model as the *main* model nothing like this happens: it writes the command
into its answer and waits for you to paste it. The productive split is a tool-capable model
for hands (read/write/run) and a research model for thinking, both behind this gateway.

</details>

## Configuration

The gateway reads `config.json` by default. Use `--config /path/to/config.json` to select another file. This excerpt shows the main settings:

```json
{
  "listen": { "host": "127.0.0.1", "port": 7868 },
  "api_key": "sk-your-client-key",
  "admin_password": "your-panel-password",
  "data_dir": "./data",
  "upstream": { "timeout_seconds": 120 },
  "pool": { "hard_cooldown": "12h", "soft_cooldown": "60s", "max_rotate": 3 },
  "prompts": {},
  "mcp": { "enabled": true, "path": "/mcp" },
  "preview": { "enabled": true, "dir": "./data/preview", "path": "/preview/" },
  "localfs": { "enabled": false, "root": "", "allow_write": false, "allow_exec": false }
}
```

Environment overrides: `FSGW_LISTEN_HOST`, `FSGW_LISTEN_PORT`, `FSGW_API_KEY`, `FSGW_ADMIN_PASSWORD`, and `FSGW_DATA_DIR`. Non-loopback listeners require both a gateway key and an admin password at startup.

Prompt, password, client key, and cooldown updates can take effect while running. Changes to listeners, mounted routes, and local tool configuration require a restart.

## Development

```bash
go build -o fsgw ./cmd/gateway
go vet ./...
go test -race ./...
```

For the browser smoke test, install Playwright and Chromium, then run `python3 tools/panel-smoke.py http://127.0.0.1:7868` against a running instance.

## License

[MIT](LICENSE). See the license for its permissions, warranty terms, and liability provisions.

<a id="disclaimer"></a>

## Disclaimer

- This is an **unofficial interface adapter for technical exchange**, with no affiliation with FutureSearch. It does not provide accounts, API keys, model weights, or permission to use upstream services. Brand and model names identify upstream offerings; they do not imply endorsement or independent verification of the underlying model's identity.
- Use only accounts and API keys you are authorized to use, in compliance with the applicable [FutureSearch terms](https://futuresearch.ai/terms/). Account pools and tenant features are for authorized scenarios; do not use them to bypass access or billing restrictions, or resell services without permission. The general terms restrict multiple accounts held by the same user and commercial use; applicable supplemental terms or separate authorization must also be considered.
- Model availability, outputs, and actual charges are determined by the upstream service. Token usage and tenant quota deductions in this gateway are estimates, not the platform's actual billing records.
- The MIT license's “as is” terms and liability limitations apply only to the extent permitted by law. This statement does not replace required authorization or exclude responsibilities that cannot legally be waived.
