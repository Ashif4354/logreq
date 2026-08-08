# logreq

A fake backend that logs every incoming HTTP request to disk. Point any client at it
and read back exactly what it sent — method, path, headers, and a decoded body — with
no real server in the loop.

## Run

```bash
uv sync                       # add --extra protobuf / --extra compression as needed
uv run main.py                # http://0.0.0.0:8081
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

One line per request, nothing else:

```
#00001 POST /v1/traces <- 127.0.0.1:52730 131B protobuf->json -> 200
#00002 GET  /health    <- 127.0.0.1:57696 0B   empty          -> 200
```

Bodies stay on disk. Pass `--summary` if you also want trace payloads unpacked into
the console — service, span names, and which attribute conventions the client is
using. Prompt, completion, input and output attributes are omitted from that view;
they're huge and often sensitive.

## Proxy Mode

`logreq` can operate in two modes:
1. **Mock Mode (Default)**: Logs incoming requests and returns protocol-correct mock responses.
2. **Proxy Mode**: Forwards incoming requests to a target server, logs both request and response details, and returns the target's exact response back to the client.

To enable **Proxy Mode**, set `PROXY_TARGET` in `.env` or as an environment variable, or pass `--proxy-target`:

```bash
# Via .env file:
# Create .env and set: PROXY_TARGET=http://localhost:8091
uv run main.py

# Via environment variable:
PROXY_TARGET=https://apm-rx.atatus.com uv run main.py

# Via CLI flag:
uv run main.py --proxy-target http://localhost:8091
```

## Flags

| Flag | Default | Purpose |
|---|---|---|
| `--host` / `--port` | `0.0.0.0` / `8081` | bind address |
| `--proxy-target URL` | `PROXY_TARGET` env | proxy target server to forward all requests to |
| `--log-dir` | `logs` | where sessions are written |
| `--status N` | — | force a status on every response (retry testing) |
| `--delay S` | `0` | stall S seconds before responding (timeout testing) |
| `--max-body N` | unlimited | truncate text bodies to N chars |
| `--summary` | off | also print a per-span summary of trace payloads |

## Docker

Build and run using `Dockerfile.python`:

```bash
# Build the Docker image
docker build -f Dockerfile.python -t logreq:latest .

# Run in Mock Mode
docker run -p 8081:8081 logreq:latest

# Run in Proxy Mode via environment variable
docker run -p 8081:8081 -e PROXY_TARGET="http://host.docker.internal:8091" logreq:latest
```

## Notes

- Bodies are held in memory before writing; not built for firehose volumes.
- Everything is captured verbatim, including auth headers and request content. Treat
  `logs/` as sensitive — it's gitignored by default.


