# logreq

A fake backend that logs every incoming HTTP request to disk. Point any client at it
and read back exactly what it sent — method, path, headers, and a decoded body — with
no real server in the loop.

## Architecture & Languages

`logreq` is designed to be multi-language extensible under `src/<language>`. All implementations write session logs into the central root `logs/` directory with language-tagged folder names (`YYYY-MM-DD_HH-MM-SS-<lang>`).

```
logreq/
├── logs/                         # Shared root log directory
│   ├── 2026-08-08_17-42-06-python/
│   └── 2026-08-08_17-49-33-go/
├── src/
│   ├── python/                   # Python implementation (FastAPI / Uvicorn)
│   │   ├── Dockerfile.python
│   │   ├── main.py
│   │   └── pyproject.toml
│   └── go/                       # Go implementation (net/http stdlib)
│       ├── Dockerfile.go
│       ├── main.go
│       └── go.mod
├── docker-compose.yml            # Multi-service docker orchestration
├── Makefile                      # Unified developer CLI
└── README.md
```

## Quick Start

### Go Implementation

```bash
# Run locally using Go
cd src/go && go run main.go

# Or via Makefile from root
make run-go
```

### Python Implementation

```bash
# Run locally using Python / uv
cd src/python && uv run main.py

# Or via Makefile from root
make run-python
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

## Output Structure

```
logs/
  2026-08-08_17-49-33-go/
    index.jsonl                    # one line per request, greppable
    00001_POST_v1_traces.json      # full record, sequence-ordered
    00002_GET_health.json
  2026-08-08_17-42-06-python/
    index.jsonl
    00001_POST_v1_traces.json
```

## Proxy Mode

`logreq` can operate in two modes:
1. **Mock Mode (Default)**: Logs incoming requests and returns protocol-correct mock responses.
2. **Proxy Mode**: Forwards incoming requests to a target server, logs both request and response details, and returns the target's exact response back to the client.

To enable **Proxy Mode**, set `PROXY_TARGET` in `.env` or as an environment variable, or pass `--proxy-target` / `-proxy-target`:

```bash
# Set PROXY_TARGET in .env or shell:
PROXY_TARGET=http://localhost:8091 make run-go
PROXY_TARGET=http://localhost:8091 make run-python
```

## Flags

| Python Flag | Go Flag | Default | Purpose |
|---|---|---|---|
| `--host` | `-host` | `0.0.0.0` | bind address |
| `--port` | `-port` | `8081` | bind port |
| `--proxy-target URL` | `-proxy-target URL` | `PROXY_TARGET` env | proxy target server to forward all requests to |
| `--log-dir` | `-log-dir` | `logs/` (root) | where sessions are written |
| `--status N` | `-status N` | — | force a status on every response |
| `--delay S` | `-delay S` | `0` | stall S seconds before responding |
| `--max-body N` | `-max-body N` | unlimited | truncate text bodies to N chars |

## Makefile & Docker Compose Commands

```bash
make help           # Show all available commands
make run-go         # Run Go service locally
make run-python     # Run Python service locally
make build-all      # Build Docker images for all languages
make up             # Start Python & Go containers via Docker Compose
make down           # Stop Docker Compose containers
make logs           # View live container logs
```

## Notes

- Bodies are held in memory before writing; not built for firehose volumes.
- Everything is captured verbatim, including auth headers and request content. Treat
  `logs/` as sensitive — it's gitignored by default.


