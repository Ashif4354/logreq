# logreq

A fake backend that logs every incoming HTTP request to disk. Point any client at it
and read back exactly what it sent — method, path, headers, and a decoded body — with
no real server in the loop.

## Run

```bash
uv sync                       # add --extra protobuf / --extra compression as needed
uv run main.py                # http://0.0.0.0:8001
uv run main.py --port 4318
```

## What it handles

| | |
|---|---|
| Methods | GET, POST, PUT, PATCH, DELETE, HEAD, OPTIONS — any path |
| Encodings | `gzip`, `deflate` (zlib + raw), `br`, `zstd`, `identity` |
| Bodies | JSON, NDJSON, form data, plain text, protobuf, raw binary |
| CORS | wide open, so browser clients get past preflight |

Nothing is ever dropped. A body that can't be parsed is stored as base64 plus a hex
preview, and any decode failure is recorded on the request instead of raising.

### Protobuf bodies

Requests posted to `/v1/traces`, `/v1/metrics` or `/v1/logs` are decoded into readable
JSON when the optional schema package is installed (`uv sync --extra protobuf`), with
binary id fields rewritten from base64 to hex. Those three paths also get a
protocol-correct empty success response, so clients that would otherwise treat a
mismatched reply as a failure and retry don't hammer you. Everything else is echoed.

## Output

```
logs/
  2026-08-06_07-57-25/
    index.jsonl                    # one line per request, greppable
    00001_POST_v1_traces.json      # full record, sequence-ordered
    00002_GET_health.json
  latest -> 2026-08-06_07-57-25/   # symlink to the current session
```

Each record holds the timestamp, method, full URL, client address, headers, query
params, decoded body, byte counts, detected body format, any decode error, and the
status that was returned.

```bash
jq -r 'select(.path=="/v1/traces") | .file' logs/latest/index.jsonl
jq '.body' logs/latest/00001_*.json
```

## Live console

Every request prints one line as it lands. Trace payloads additionally print a short
summary — service, span names, and which attribute conventions the client is using:

```
#00001 POST /v1/traces <- 127.0.0.1:52730 131B protobuf->json -> 200
  resource service.name=checkout-api
    span 'ChatCompletion' kind=3 trace=abc123def4567890 llm.model_name=some-model
```

Prompt, completion, input and output attributes are kept off the console — they're
huge and often sensitive. The full body is still written to disk.

## Flags

| Flag | Default | Purpose |
|---|---|---|
| `--host` / `--port` | `0.0.0.0` / `8001` | bind address |
| `--log-dir` | `logs` | where sessions are written |
| `--status N` | — | force a status on every response (retry testing) |
| `--delay S` | `0` | stall S seconds before responding (timeout testing) |
| `--max-body N` | unlimited | truncate text bodies to N chars |
| `--no-summary` | off | disable the trace summaries |

## Notes

- Bodies are held in memory before writing; not built for firehose volumes.
- Everything is captured verbatim, including auth headers and request content. Treat
  `logs/` as sensitive — it's gitignored by default.
