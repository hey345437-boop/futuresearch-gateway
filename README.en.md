# futuresearch-gateway

[中文](README.md) | English

Turn [FutureSearch](https://futuresearch.ai)'s **asynchronous task API** into an
**OpenAI-compatible endpoint + admin panel + two MCP servers**.

One binary, one config file, one data directory. That's the whole thing.

```
your client ──OpenAI-compatible──▶ futuresearch-gateway ──▶ FutureSearch v0 API
(dsh / Claude Code /                 ├─ key pool: rotation + stickiness + cooldown
 Cursor / any SDK)                   ├─ panel: accounts / models / usage / prompt injection
                                     └─ MCP: /mcp (research) · /mcp/local (your files)
```

## Quick start

```bash
docker run -d --name fsgw -p 7868:7868 -v $PWD/fsgw-data:/data \
  -e FSGW_LISTEN_HOST=0.0.0.0 \
  -e FSGW_API_KEY=sk-your-client-key \
  -e FSGW_ADMIN_PASSWORD=your-panel-password \
  ghcr.io/hey345437-boop/futuresearch-gateway:latest
```

Open `http://127.0.0.1:7868/` → paste a FutureSearch API key → done.

> **Safety gate**: when listening on a non-loopback address, both `FSGW_API_KEY` and
> `FSGW_ADMIN_PASSWORD` are **required** or the process refuses to start.

Or build from source:

```bash
make build && ./fsgw            # http://127.0.0.1:7868
```

## What you get

| | |
|---|---|
| **OpenAI endpoint** | `POST /v1/chat/completions` (stream + non-stream), `GET /v1/models` — 185 models |
| **Admin panel** | accounts / tenants & quotas / models / prompt injection / **local preview** / settings |
| **MCP (research)** | `/mcp` → `research` / `forecast` / `models` |
| **MCP (local)** | `/mcp/local` → `list_dir` / `read_file` / `search` (+ optional `write_file` / `mkdir` / `run_command`) |
| **Local preview** | serve a directory at `/preview/` so generated HTML is instantly viewable |

## Models

Two ways, both supported:

| Form | Example | Notes |
|---|---|---|
| **Presets** | `agent-low` / `agent-medium` / `agent-high`, `multi-agent-*`, `forecast` | the platform picks the model; the tier is research depth |
| **Family + tier** | `claude-fable-5` + `reasoning_effort: max` | you pick the underlying model |
| **Full enum** | `claude-fable-5-max` | tier baked into the name — **use this behind a third-party gateway** |

## Design notes (the part that actually matters)

1. **Keepalive must be a real event.** Client idle watchdogs count *parsed stream events*,
   not bytes. Empty-delta heartbeats parse to nothing, so a 5-minute task still trips
   `stream idle timeout after 300000ms` while the gateway logs show it writing happily.
   This gateway emits task progress as `reasoning_content` — a real event, and it makes the
   wait visible in the client's thinking pane.
2. **Never fake `[DONE]` on failure.** `[DONE]` means "finished cleanly". Appending it to a
   truncated stream turns a failure into a silent half-answer. Errors emit an `error` frame only.
3. **143 enums collapse into family × tier**, with the full enum names still callable.
4. **Prompt injection**, because the upstream API has no `system` field — the only lever is
   the front of the task text.
5. **`usage` is an estimate.** The upstream bills per task and returns no token counts.
   Don't bill by token against this.

## Limitations

- **No tool calling.** The upstream API has no tool-callback surface, so its agent cannot
  call your local tools. It's a *research subroutine*, not a coding agent. Local hands come
  from `/mcp/local` (a separate MCP server, on purpose).
- **No image input.** In the whole OpenAPI spec, `png`/`jpeg`/`vision`/`multimodal`/`base64`
  appear **zero** times; uploads are CSV/JSON only. Requests with images fail loudly with
  `images_not_supported` instead of silently dropping the image.
- **No real multi-turn memory.** Every turn is a fresh task; history is folded into a
  truncated context preamble.

## License

MIT. Unofficial wrapper around FutureSearch's public API; not affiliated with FutureSearch.
