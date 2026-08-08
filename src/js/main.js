const http = require('http');
const https = require('https');
const fs = require('fs');
const path = require('path');
const zlib = require('zlib');
const util = require('util');
const { URL } = require('url');

const gunzipAsync = util.promisify(zlib.gunzip);
const inflateAsync = util.promisify(zlib.inflate);
const inflateRawAsync = util.promisify(zlib.inflateRaw);

// Reusable connection-pooled HTTP/HTTPS agents for ultra-high proxy throughput
const httpAgent = new http.Agent({ keepAlive: true, maxSockets: 500, keepAliveMsecs: 10000 });
const httpsAgent = new https.Agent({ keepAlive: true, maxSockets: 500, keepAliveMsecs: 10000 });

// --------------------------------------------------------------------------
// .env loader
// --------------------------------------------------------------------------
function loadDotEnv(envPath) {
  if (!fs.existsSync(envPath)) return;
  try {
    const content = fs.readFileSync(envPath, 'utf8');
    const lines = content.split('\n');
    for (const line of lines) {
      const trimmed = line.trim();
      if (!trimmed || trimmed.startsWith('#')) continue;
      const idx = trimmed.indexOf('=');
      if (idx !== -1) {
        const key = trimmed.slice(0, idx).trim();
        let val = trimmed.slice(idx + 1).trim();
        if ((val.startsWith('"') && val.endsWith('"')) || (val.startsWith("'") && val.endsWith("'"))) {
          val = val.slice(1, -1);
        }
        if (!process.env[key]) {
          process.env[key] = val;
        }
      }
    }
  } catch (err) {
    // Ignore read errors
  }
}

loadDotEnv(path.join(__dirname, '.env'));
loadDotEnv(path.join(process.cwd(), '.env'));

// --------------------------------------------------------------------------
// CLI args parsing
// --------------------------------------------------------------------------
function parseArgs() {
  const args = process.argv.slice(2);
  const rootLogDir = path.resolve(__dirname, '../../logs');
  
  const config = {
    host: '0.0.0.0',
    port: 8081,
    logDir: rootLogDir,
    status: 0,
    delay: 0,
    maxBody: 0,
    proxyTarget: process.env.PROXY_TARGET || ''
  };

  for (let i = 0; i < args.length; i++) {
    const arg = args[i];
    const val = args[i + 1];

    if (arg === '--host' || arg === '-host') { config.host = val; i++; }
    else if (arg === '--port' || arg === '-port') { config.port = parseInt(val, 10); i++; }
    else if (arg === '--log-dir' || arg === '-log-dir') { config.logDir = path.resolve(val); i++; }
    else if (arg === '--status' || arg === '-status') { config.status = parseInt(val, 10); i++; }
    else if (arg === '--delay' || arg === '-delay') { config.delay = parseFloat(val); i++; }
    else if (arg === '--max-body' || arg === '-max-body') { config.maxBody = parseInt(val, 10); i++; }
    else if (arg === '--proxy-target' || arg === '-proxy-target') { config.proxyTarget = val; i++; }
  }

  if (config.proxyTarget) {
    config.proxyTarget = config.proxyTarget.trim().replace(/\/+$/, '');
  }

  return config;
}

const config = parseArgs();

// --------------------------------------------------------------------------
// Session state
// --------------------------------------------------------------------------
function formatDate(d) {
  const pad = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}_${pad(d.getHours())}-${pad(d.getMinutes())}-${pad(d.getSeconds())}`;
}

const session = `${formatDate(new Date())}-js`;
const sessionDir = path.join(config.logDir, session);
fs.mkdirSync(sessionDir, { recursive: true });
const indexPath = path.join(sessionDir, 'index.jsonl');

let seq = 0;

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------
function otlpSignal(reqPath) {
  const tail = '/' + reqPath.replace(/^\/+|\/+$/g, '');
  for (const sig of ['traces', 'metrics', 'logs']) {
    if (tail.endsWith(`/v1/${sig}`)) return sig;
  }
  return null;
}

async function decompressBody(raw, encoding) {
  const enc = (encoding || '').trim().toLowerCase();
  if (!raw || !raw.length || !enc || enc === 'identity') {
    return { body: raw, error: null };
  }
  try {
    if (enc === 'gzip') {
      const body = await gunzipAsync(raw);
      return { body, error: null };
    }
    if (enc === 'deflate') {
      try {
        const body = await inflateAsync(raw);
        return { body, error: null };
      } catch (e) {
        const body = await inflateRawAsync(raw);
        return { body, error: null };
      }
    }
  } catch (err) {
    return { body: raw, error: `${enc} decompression failed: ${err.message}` };
  }
  return { body: raw, error: `unsupported encoding: ${enc}` };
}

function parseBody(raw, contentType) {
  if (!raw || !raw.length) return { parsed: null, format: 'empty' };
  
  const ctype = (contentType || '').split(';')[0].trim().toLowerCase();

  if (ctype.includes('protobuf')) {
    return {
      parsed: {
        _base64: raw.toString('base64'),
        _hex: raw.subarray(0, 64).toString('hex')
      },
      format: 'protobuf-raw'
    };
  }

  const text = raw.toString('utf8');

  // Try JSON
  try {
    return { parsed: JSON.parse(text), format: 'json' };
  } catch (e) {}

  // Try NDJSON
  if (ctype.includes('ndjson') || (text.includes('\n') && (text.trim().startsWith('{') || text.trim().startsWith('[')))) {
    const lines = text.split('\n').map(l => l.trim()).filter(Boolean);
    try {
      const parsedLines = lines.map(l => JSON.parse(l));
      if (parsedLines.length > 0) {
        return { parsed: parsedLines, format: 'ndjson' };
      }
    } catch (e) {}
  }

  // Check binary
  if (raw.includes(0)) {
    return {
      parsed: { _base64: raw.toString('base64') },
      format: 'binary'
    };
  }

  return { parsed: text, format: 'text' };
}

function safeName(reqPath) {
  const cleaned = reqPath.replace(/^\/+|\/+$/g, '').replace(/\//g, '_') || 'root';
  const keep = cleaned.replace(/[^a-zA-Z0-9._-]/g, '-');
  return keep.slice(0, 80);
}

// Asynchronous non-blocking file writer
async function writeRecord(record, filename) {
  try {
    await fs.promises.writeFile(path.join(sessionDir, filename), JSON.stringify(record, null, 2), 'utf8');
    
    const indexLine = {
      seq: record.seq,
      ts: record.timestamp,
      method: record.method,
      path: record.path,
      status: record.response_status,
      bytes: record.body_bytes,
      file: filename
    };
    await fs.promises.appendFile(indexPath, JSON.stringify(indexLine) + '\n', 'utf8');
  } catch (err) {
    console.error(`[LOG ERROR] Failed to write log record: ${err.message}`);
  }
}

// --------------------------------------------------------------------------
// HTTP Server Handler
// --------------------------------------------------------------------------
const server = http.createServer((req, res) => {
  const received = new Date();
  const chunks = [];

  req.on('data', chunk => chunks.push(chunk));
  req.on('end', async () => {
    const raw = Buffer.concat(chunks);
    const reqUrl = new URL(req.url, `http://${req.headers.host || 'localhost'}`);
    const fullPath = reqUrl.pathname.replace(/^\/+/, '');
    
    const { body, error: decodeError } = await decompressBody(raw, req.headers['content-encoding']);
    const contentType = req.headers['content-type'] || '';
    let { parsed, format: bodyFormat } = parseBody(body, contentType);

    if (config.maxBody && typeof parsed === 'string' && parsed.length > config.maxBody) {
      parsed = parsed.slice(0, config.maxBody) + `... [truncated, ${parsed.length} chars]`;
    }

    seq++;
    const currentSeq = seq;
    const signal = otlpSignal(fullPath);

    // Set CORS headers
    res.setHeader('Access-Control-Allow-Origin', '*');
    res.setHeader('Access-Control-Allow-Methods', '*');
    res.setHeader('Access-Control-Allow-Headers', '*');
    res.setHeader('Access-Control-Expose-Headers', '*');

    if (req.method === 'OPTIONS') {
      res.statusCode = 204;
      res.end();
      return;
    }

    const isProxy = Boolean(config.proxyTarget);
    let status = config.status || 200;
    let targetUrl = null;
    let elapsedMs = 0;
    let proxyError = null;
    let proxyRespHeaders = {};
    let proxyRespBody = Buffer.alloc(0);

    if (isProxy) {
      const targetBase = config.proxyTarget;
      const pathPart = '/' + fullPath;
      const queryPart = reqUrl.search;
      targetUrl = `${targetBase}${pathPart}${queryPart}`;

      const fwdHeaders = { ...req.headers };
      delete fwdHeaders.host;
      delete fwdHeaders['content-length'];
      delete fwdHeaders['transfer-encoding'];
      delete fwdHeaders.connection;

      const isHttps = targetUrl.startsWith('https:');
      const clientModule = isHttps ? https : http;
      const targetAgent = isHttps ? httpsAgent : httpAgent;

      const startTime = process.hrtime.bigint();
      try {
        const result = await new Promise((resolve, reject) => {
          const proxyReq = clientModule.request(targetUrl, {
            method: req.method,
            headers: fwdHeaders,
            agent: targetAgent,
            timeout: 60000
          }, (proxyRes) => {
            const respChunks = [];
            proxyRes.on('data', c => respChunks.push(c));
            proxyRes.on('end', () => {
              resolve({
                statusCode: proxyRes.statusCode,
                headers: proxyRes.headers,
                body: Buffer.concat(respChunks)
              });
            });
          });

          proxyReq.on('error', err => reject(err));
          proxyReq.on('timeout', () => {
            proxyReq.destroy();
            reject(new Error('Proxy request timed out'));
          });

          if (raw.length) {
            proxyReq.write(raw);
          }
          proxyReq.end();
        });

        const endTime = process.hrtime.bigint();
        elapsedMs = Number(endTime - startTime) / 1e6;

        status = config.status || result.statusCode;
        proxyRespHeaders = result.headers;
        proxyRespBody = result.body;
      } catch (err) {
        const endTime = process.hrtime.bigint();
        elapsedMs = Number(endTime - startTime) / 1e6;

        proxyError = `Proxy forwarding error: ${err.message}`;
        status = config.status || 502;
        proxyRespHeaders = { 'content-type': 'application/json' };
        proxyRespBody = Buffer.from(JSON.stringify({ error: 'Bad Gateway', detail: err.message }));
      }
    }

    // Query parameters object
    const queryParams = {};
    reqUrl.searchParams.forEach((v, k) => { queryParams[k] = v; });

    const record = {
      seq: currentSeq,
      timestamp: received.toISOString(),
      method: req.method,
      url: req.url,
      path: '/' + fullPath,
      client: req.socket.remoteAddress ? `${req.socket.remoteAddress}:${req.socket.remotePort}` : null,
      http_version: `HTTP/${req.httpVersion}`,
      otlp_signal: signal,
      body_format: bodyFormat,
      body_bytes: raw.length,
      decoded_bytes: body.length,
      decode_error: decodeError,
      headers: req.headers,
      query_params: queryParams,
      body: parsed,
      response_status: status
    };

    if (isProxy) {
      record.proxy_target = targetUrl;
      record.upstream_latency_ms = Math.round(elapsedMs * 100) / 100;
      if (proxyError) record.proxy_error = proxyError;
    }

    const filename = `${String(currentSeq).padStart(5, '0')}_${req.method}_${safeName(fullPath)}.json`;
    
    // Non-blocking background log write
    writeRecord(record, filename);

    if (isProxy) {
      console.log(`#${String(currentSeq).padStart(5, '0')} ${req.method} /${fullPath} <- ${record.client} ${raw.length}B ${bodyFormat} => ${targetUrl} -> ${status} (${elapsedMs.toFixed(1)}ms)`);
    } else {
      console.log(`#${String(currentSeq).padStart(5, '0')} ${req.method} /${fullPath} <- ${record.client} ${raw.length}B ${bodyFormat} -> ${status}`);
    }

    if (decodeError) console.log(`  ! ${decodeError}`);
    if (proxyError) console.log(`  ! ${proxyError}`);

    const respond = () => {
      if (isProxy) {
        for (const [k, v] of Object.entries(proxyRespHeaders)) {
          const lk = k.toLowerCase();
          if (lk === 'content-encoding' || lk === 'content-length' || lk === 'transfer-encoding' || lk === 'connection') {
            continue;
          }
          res.setHeader(k, v);
        }
        res.statusCode = status;
        res.end(proxyRespBody);
        return;
      }

      // Mock Mode Response
      if (req.method === 'HEAD' || req.method === 'OPTIONS') {
        res.statusCode = status === 200 ? 204 : status;
        res.end();
        return;
      }

      if (signal) {
        if (contentType.includes('protobuf')) {
          res.setHeader('Content-Type', 'application/x-protobuf');
          res.statusCode = status;
          res.end();
          return;
        }
        res.setHeader('Content-Type', 'application/json');
        res.statusCode = status;
        res.end(JSON.stringify({}));
        return;
      }

      res.setHeader('Content-Type', 'application/json');
      res.statusCode = status;
      if (req.method === 'GET') {
        res.end(JSON.stringify(record));
      } else {
        res.end(JSON.stringify({ ok: status < 400, seq: currentSeq }));
      }
    };

    if (config.delay) {
      setTimeout(respond, config.delay * 1000);
    } else {
      respond();
    }
  });
});

server.listen(config.port, config.host, () => {
  if (config.proxyTarget) {
    console.log(`logreq session ${session} -> ${sessionDir} [PROXY MODE -> ${config.proxyTarget}]`);
  } else {
    console.log(`logreq session ${session} -> ${sessionDir} [MOCK MODE]`);
  }
});
