"""logreq — a fake backend that logs every incoming HTTP request to disk.

Point any client at it and read back exactly what it sent, with no real server in
the loop.

Handles:
  * any method, any path (catch-all)
  * gzip / deflate / br / zstd request bodies
  * JSON, NDJSON, protobuf, form data, text and raw binary
  * the three trace/metric/log ingest paths with protocol-correct responses, so
    clients see a clean success instead of retrying
  * chaos knobs (--status, --delay) to exercise client retry paths

Run:
    uv run main.py                        # http://0.0.0.0:8081
    uv run main.py --port 4318
    uv run main.py --status 503 --delay 2 # make the client sweat
"""

from __future__ import annotations

import argparse
import asyncio
import base64
import binascii
import gzip
import json
import zlib
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from fastapi import FastAPI, Request
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import JSONResponse, Response

# --------------------------------------------------------------------------
# optional decoders — everything degrades gracefully when they're missing
# --------------------------------------------------------------------------

try:  # pip install brotli
    import brotli
except ImportError:
    brotli = None

try:  # pip install zstandard
    import zstandard
except ImportError:
    zstandard = None

try:  # uv sync --extra protobuf
    from google.protobuf.json_format import MessageToDict
    from opentelemetry.proto.collector.logs.v1.logs_service_pb2 import (
        ExportLogsServiceRequest,
    )
    from opentelemetry.proto.collector.metrics.v1.metrics_service_pb2 import (
        ExportMetricsServiceRequest,
    )
    from opentelemetry.proto.collector.trace.v1.trace_service_pb2 import (
        ExportTraceServiceRequest,
    )

    OTLP_PROTO = {
        "traces": ExportTraceServiceRequest,
        "metrics": ExportMetricsServiceRequest,
        "logs": ExportLogsServiceRequest,
    }
except ImportError:  # protobuf bodies get logged as base64 instead
    MessageToDict = None
    OTLP_PROTO = {}


PROTOBUF_TYPES = ("application/x-protobuf", "application/protobuf")

# protobuf `bytes` fields decode to base64, which is unreadable for ids
ID_FIELDS = {"trace_id", "traceId", "span_id", "spanId", "parent_span_id", "parentSpanId"}


# --------------------------------------------------------------------------
# state
# --------------------------------------------------------------------------


@dataclass
class Config:
    log_dir: Path = Path("logs")
    status: int | None = None
    delay: float = 0.0
    summarize: bool = True
    max_body: int = 0  # 0 = unlimited


@dataclass
class State:
    config: Config = field(default_factory=Config)
    session: str = ""
    session_dir: Path = Path(".")
    index_path: Path = Path(".")
    seq: int = 0
    lock: asyncio.Lock = field(default_factory=asyncio.Lock)


state = State()
app = FastAPI(title="logreq", docs_url=None, redoc_url=None, openapi_url=None)

# browser-based clients need preflight to pass before they'll POST anything
app.add_middleware(
    CORSMiddleware,
    allow_origins=["*"],
    allow_methods=["*"],
    allow_headers=["*"],
    expose_headers=["*"],
)


# --------------------------------------------------------------------------
# body decoding
# --------------------------------------------------------------------------


def decompress(raw: bytes, encoding: str) -> tuple[bytes, str | None]:
    """Return (body, error). Unknown or failed encodings return the raw bytes."""
    encoding = (encoding or "").strip().lower()
    if not raw or encoding in ("", "identity"):
        return raw, None
    try:
        if encoding == "gzip":
            return gzip.decompress(raw), None
        if encoding == "deflate":
            try:
                return zlib.decompress(raw), None
            except zlib.error:  # raw deflate, no zlib header
                return zlib.decompress(raw, -zlib.MAX_WBITS), None
        if encoding == "br":
            if brotli is None:
                return raw, "brotli not installed (pip install brotli)"
            return brotli.decompress(raw), None
        if encoding == "zstd":
            if zstandard is None:
                return raw, "zstandard not installed (pip install zstandard)"
            return zstandard.ZstdDecompressor().decompress(raw), None
    except Exception as exc:
        return raw, f"{encoding} decompression failed: {exc}"
    return raw, f"unknown content-encoding: {encoding}"


def hexify_ids(node: Any) -> Any:
    """Rewrite base64 trace/span ids to hex, in place, at any depth."""
    if isinstance(node, dict):
        for key, value in node.items():
            if key in ID_FIELDS and isinstance(value, str):
                try:
                    node[key] = base64.b64decode(value, validate=True).hex()
                except (binascii.Error, ValueError):
                    pass
            else:
                hexify_ids(value)
    elif isinstance(node, list):
        for item in node:
            hexify_ids(item)
    return node


def otlp_signal(path: str) -> str | None:
    """'traces' | 'metrics' | 'logs' for the known ingest paths, else None."""
    tail = "/" + path.strip("/")
    for signal in ("traces", "metrics", "logs"):
        if tail.endswith(f"/v1/{signal}"):
            return signal
    return None


def parse_body(raw: bytes, content_type: str, path: str) -> tuple[Any, str]:
    """Return (parsed_body, encoding_label) — never raises."""
    if not raw:
        return None, "empty"

    ctype = (content_type or "").split(";")[0].strip().lower()

    if any(ctype.startswith(p) for p in PROTOBUF_TYPES):
        signal = otlp_signal(path)
        message = OTLP_PROTO.get(signal) if signal else None
        if message is not None:
            try:
                decoded = message()
                decoded.ParseFromString(raw)
                return (
                    hexify_ids(MessageToDict(decoded, preserving_proto_field_name=True)),
                    "protobuf->json",
                )
            except Exception as exc:
                return {
                    "_error": f"protobuf decode failed: {exc}",
                    "_base64": base64.b64encode(raw).decode(),
                }, "protobuf-undecodable"
        return {
            "_note": "protobuf body; run `uv sync --extra protobuf` to decode it",
            "_hex_preview": binascii.hexlify(raw[:64]).decode(),
            "_base64": base64.b64encode(raw).decode(),
        }, "protobuf-raw"

    text = raw.decode("utf-8", errors="replace")

    if ctype == "application/x-ndjson" or text.count("\n") > 1 and text.lstrip()[:1] in "{[":
        lines = [ln for ln in text.splitlines() if ln.strip()]
        try:
            return [json.loads(ln) for ln in lines], "ndjson"
        except json.JSONDecodeError:
            pass

    try:
        return json.loads(text), "json"
    except json.JSONDecodeError:
        pass

    if "�" in text:  # undecodable bytes — keep them recoverable
        return {"_base64": base64.b64encode(raw).decode()}, "binary"
    return text, "text"


# --------------------------------------------------------------------------
# trace summary — the "what is this client speaking" view
# --------------------------------------------------------------------------

# attribute namespaces worth calling out, since a client usually picks one
DIALECT_PREFIXES = {
    "gen_ai.": "gen_ai",
    "openinference.": "openinference",
    "llm.": "llm",
    "traceloop.": "traceloop",
}


def flatten_attributes(attrs: list[dict] | None) -> dict[str, Any]:
    """KeyValue attribute list -> flat dict."""
    out: dict[str, Any] = {}
    for item in attrs or []:
        key = item.get("key")
        value = item.get("value") or {}
        if not key:
            continue
        for vkey in (
            "stringValue",
            "string_value",
            "intValue",
            "int_value",
            "doubleValue",
            "double_value",
            "boolValue",
            "bool_value",
        ):
            if vkey in value:
                out[key] = value[vkey]
                break
        else:
            out[key] = value
    return out


def summarize_traces(payload: Any) -> list[str]:
    """One line per span, plus the attribute dialects seen."""
    if not isinstance(payload, dict):
        return []
    resource_spans = payload.get("resourceSpans") or payload.get("resource_spans") or []
    lines: list[str] = []
    dialects: set[str] = set()

    for resource_span in resource_spans:
        resource = resource_span.get("resource") or {}
        res_attrs = flatten_attributes(resource.get("attributes"))
        service = res_attrs.get("service.name", "<no service.name>")
        others = [f"{k}={v}" for k, v in res_attrs.items() if k != "service.name"][:3]
        lines.append(f"  resource service.name={service} {' '.join(others)}".rstrip())

        scope_spans = (
            resource_span.get("scopeSpans")
            or resource_span.get("scope_spans")
            or resource_span.get("instrumentationLibrarySpans")
            or []
        )
        for scope_span in scope_spans:
            for span in scope_span.get("spans") or []:
                attrs = flatten_attributes(span.get("attributes"))
                for key in attrs:
                    for prefix, label in DIALECT_PREFIXES.items():
                        if key.startswith(prefix):
                            dialects.add(label)
                kind = span.get("kind", span.get("spanKind", "?"))
                name = span.get("name", "<unnamed>")
                trace_id = (span.get("traceId") or span.get("trace_id") or "")[:16]
                interesting = {
                    k: v
                    for k, v in attrs.items()
                    if k.startswith(("gen_ai.", "llm.", "openinference.", "traceloop."))
                    and "prompt" not in k
                    and "completion" not in k
                    and "input" not in k
                    and "output" not in k
                }
                extra = " ".join(f"{k}={v}" for k, v in list(interesting.items())[:4])
                lines.append(f"    span {name!r} kind={kind} trace={trace_id} {extra}".rstrip())

    if dialects:
        lines.append(f"  dialects: {', '.join(sorted(dialects))}")
    return lines


# --------------------------------------------------------------------------
# persistence
# --------------------------------------------------------------------------


def safe_name(path: str) -> str:
    cleaned = path.strip("/").replace("/", "_") or "root"
    keep = "".join(c if c.isalnum() or c in "._-" else "-" for c in cleaned)
    return keep[:80]


def write_record(record: dict, filename: str) -> None:
    """Blocking disk I/O — always called via asyncio.to_thread."""
    (state.session_dir / filename).write_text(
        json.dumps(record, indent=2, ensure_ascii=False, default=str), encoding="utf-8"
    )
    index_line = {
        "seq": record["seq"],
        "ts": record["timestamp"],
        "method": record["method"],
        "path": record["path"],
        "status": record["response_status"],
        "bytes": record["body_bytes"],
        "file": filename,
    }
    with (state.index_path).open("a", encoding="utf-8") as handle:
        handle.write(json.dumps(index_line) + "\n")


# --------------------------------------------------------------------------
# the one route to rule them all
# --------------------------------------------------------------------------


@app.api_route(
    "/{full_path:path}",
    methods=["GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"],
)
async def catch_all(full_path: str, request: Request) -> Response:
    received = datetime.now(timezone.utc)
    raw = await request.body()

    body, decode_error = decompress(raw, request.headers.get("content-encoding", ""))
    content_type = request.headers.get("content-type", "")
    parsed, body_format = parse_body(body, content_type, full_path)

    if state.config.max_body and isinstance(parsed, str) and len(parsed) > state.config.max_body:
        parsed = parsed[: state.config.max_body] + f"... [truncated, {len(parsed)} chars]"

    async with state.lock:
        state.seq += 1
        seq = state.seq

    signal = otlp_signal(full_path)
    status = state.config.status or 200

    record = {
        "seq": seq,
        "timestamp": received.isoformat(),
        "method": request.method,
        "url": str(request.url),
        "path": "/" + full_path,
        "client": f"{request.client.host}:{request.client.port}" if request.client else None,
        "http_version": request.scope.get("http_version"),
        "otlp_signal": signal,
        "body_format": body_format,
        "body_bytes": len(raw),
        "decoded_bytes": len(body),
        "decode_error": decode_error,
        "headers": dict(request.headers),
        "query_params": dict(request.query_params),
        "body": parsed,
        "response_status": status,
    }

    filename = f"{seq:05d}_{request.method}_{safe_name(full_path)}.json"
    await asyncio.to_thread(write_record, record, filename)

    line = (
        f"#{seq:05d} {request.method} /{full_path} "
        f"<- {record['client']} {len(raw)}B {body_format} -> {status}"
    )
    print(line, flush=True)
    if decode_error:
        print(f"  ! {decode_error}", flush=True)
    if state.config.summarize and signal == "traces":
        for summary_line in summarize_traces(parsed):
            print(summary_line, flush=True)

    if state.config.delay:
        await asyncio.sleep(state.config.delay)

    return build_response(request, signal, content_type, status, record)


def build_response(
    request: Request, signal: str | None, content_type: str, status: int, record: dict
) -> Response:
    """Answer the way the client expects, so exporters don't retry forever."""
    if request.method in ("HEAD", "OPTIONS"):
        return Response(status_code=status if status != 200 else 204)

    if signal:
        ctype = (content_type or "").split(";")[0].strip().lower()
        if any(ctype.startswith(p) for p in PROTOBUF_TYPES):
            # an empty ExportXServiceResponse serialises to zero bytes
            return Response(
                content=b"", media_type="application/x-protobuf", status_code=status
            )
        return JSONResponse({}, status_code=status)

    if request.method == "GET":
        return JSONResponse(record, status_code=status)
    return JSONResponse({"ok": status < 400, "seq": record["seq"]}, status_code=status)


# --------------------------------------------------------------------------
# entrypoint
# --------------------------------------------------------------------------


def start_session(config: Config) -> None:
    state.config = config
    state.session = datetime.now().strftime("%Y-%m-%d_%H-%M-%S")
    state.session_dir = config.log_dir / state.session
    state.session_dir.mkdir(parents=True, exist_ok=True)
    state.index_path = state.session_dir / "index.jsonl"

    latest = config.log_dir / "latest"
    latest.unlink(missing_ok=True)
    try:
        latest.symlink_to(state.session_dir.resolve(), target_is_directory=True)
    except OSError:
        pass


def main() -> None:
    parser = argparse.ArgumentParser(description="fake backend that logs every request")
    parser.add_argument("--host", default="0.0.0.0")
    parser.add_argument("--port", type=int, default=8081)
    parser.add_argument("--log-dir", default="logs", type=Path)
    parser.add_argument("--status", type=int, help="force this status on every response")
    parser.add_argument("--delay", type=float, default=0.0, help="seconds to stall")
    parser.add_argument("--no-summary", action="store_true", help="skip trace summaries")
    parser.add_argument("--max-body", type=int, default=0, help="truncate text bodies")
    args = parser.parse_args()

    start_session(
        Config(
            log_dir=args.log_dir,
            status=args.status,
            delay=args.delay,
            summarize=not args.no_summary,
            max_body=args.max_body,
        )
    )

    print(f"logreq session {state.session} -> {state.session_dir}", flush=True)
    if not OTLP_PROTO:
        print("note: no protobuf schemas installed, protobuf bodies stay base64", flush=True)

    import uvicorn

    uvicorn.run(app, host=args.host, port=args.port, access_log=False)


if __name__ == "__main__":
    main()
