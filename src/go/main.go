package main

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Config struct {
	LogDir      string
	Status      int
	Delay       float64
	MaxBody     int
	ProxyTarget string
}

type State struct {
	Config     Config
	Session    string
	SessionDir string
	IndexPath  string
	Seq        int64
	IndexMutex sync.Mutex
}

var state State

// Shared HTTP Client with connection pooling for ultra-high throughput
var sharedHTTPClient = &http.Client{
	Timeout: 60 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        1000,
		MaxIdleConnsPerHost: 200,
		IdleConnTimeout:     90 * time.Second,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func loadDotEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			val = strings.Trim(val, `"'`)
			if os.Getenv(key) == "" {
				os.Setenv(key, val)
			}
		}
	}
}

func main() {
	loadDotEnv(".env")
	loadDotEnv("../../.env")

	execPath, err := os.Executable()
	var defaultLogDir string
	if err == nil {
		defaultLogDir = filepath.Join(filepath.Dir(execPath), "..", "..", "logs")
	} else {
		defaultLogDir = "logs"
	}
	absLogDir, err := filepath.Abs(filepath.Join("..", "..", "logs"))
	if err == nil {
		defaultLogDir = absLogDir
	}

	hostFlag := flag.String("host", "0.0.0.0", "bind host")
	portFlag := flag.Int("port", 8081, "bind port")
	logDirFlag := flag.String("log-dir", defaultLogDir, "where sessions are written")
	statusFlag := flag.Int("status", 0, "force a status code on every response")
	delayFlag := flag.Float64("delay", 0.0, "seconds to delay response")
	maxBodyFlag := flag.Int("max-body", 0, "truncate text bodies to N chars")
	proxyTargetFlag := flag.String("proxy-target", "", "target URL to proxy to")
	flag.Parse()

	proxyTarget := *proxyTargetFlag
	if proxyTarget == "" {
		proxyTarget = os.Getenv("PROXY_TARGET")
	}
	proxyTarget = strings.TrimRight(strings.TrimSpace(proxyTarget), "/")

	state.Config = Config{
		LogDir:      *logDirFlag,
		Status:      *statusFlag,
		Delay:       *delayFlag,
		MaxBody:     *maxBodyFlag,
		ProxyTarget: proxyTarget,
	}

	startSession()

	if state.Config.ProxyTarget != "" {
		fmt.Printf("logreq session %s -> %s [PROXY MODE -> %s]\n", state.Session, state.SessionDir, state.Config.ProxyTarget)
	} else {
		fmt.Printf("logreq session %s -> %s [MOCK MODE]\n", state.Session, state.SessionDir)
	}

	addr := fmt.Sprintf("%s:%d", *hostFlag, *portFlag)
	http.HandleFunc("/", catchAllHandler)

	server := &http.Server{
		Addr: addr,
	}
	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}

var tzRegex = regexp.MustCompile(`^([+-])(\d{1,2}):(\d{2})$`)

func parseTimezoneEnv() time.Time {
	tzStr := strings.TrimSpace(os.Getenv("TIMEZONE"))
	matches := tzRegex.FindStringSubmatch(tzStr)
	if matches == nil {
		return time.Now().UTC()
	}

	signStr := matches[1]
	var hours, mins int
	fmt.Sscanf(matches[2], "%d", &hours)
	fmt.Sscanf(matches[3], "%d", &mins)

	if hours < 0 || hours > 23 || mins < 0 || mins > 59 {
		return time.Now().UTC()
	}

	sign := 1
	if signStr == "-" {
		sign = -1
	}

	totalMinutes := sign * (hours*60 + mins)
	normSign := "+"
	if totalMinutes < 0 {
		normSign = "-"
	}
	absMins := totalMinutes
	if absMins < 0 {
		absMins = -absMins
	}
	normH := absMins / 60
	normM := absMins % 60
	normStr := fmt.Sprintf("%s%02d:%02d", normSign, normH, normM)

	loc := time.FixedZone(normStr, totalMinutes*60)
	return time.Now().In(loc)
}

func getNow() time.Time {
	return parseTimezoneEnv()
}

func startSession() {
	state.Session = getNow().Format("2006-01-02_15-04-05") + "-go"
	state.SessionDir = filepath.Join(state.Config.LogDir, state.Session)
	os.MkdirAll(state.SessionDir, 0755)
	state.IndexPath = filepath.Join(state.SessionDir, "index.jsonl")
}

func otlpSignal(path string) string {
	tail := "/" + strings.Trim(path, "/")
	for _, sig := range []string{"traces", "metrics", "logs"} {
		if strings.HasSuffix(tail, "/v1/"+sig) {
			return sig
		}
	}
	return ""
}

func decompressBody(raw []byte, encoding string) ([]byte, string) {
	enc := strings.ToLower(strings.TrimSpace(encoding))
	if len(raw) == 0 || enc == "" || enc == "identity" {
		return raw, ""
	}
	if enc == "gzip" {
		r, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return raw, fmt.Sprintf("gzip decompression failed: %v", err)
		}
		defer r.Close()
		decomp, err := io.ReadAll(r)
		if err != nil {
			return raw, fmt.Sprintf("gzip read failed: %v", err)
		}
		return decomp, ""
	}
	if enc == "deflate" {
		r := flate.NewReader(bytes.NewReader(raw))
		defer r.Close()
		decomp, err := io.ReadAll(r)
		if err != nil {
			return raw, fmt.Sprintf("deflate read failed: %v", err)
		}
		return decomp, ""
	}
	return raw, fmt.Sprintf("unsupported encoding: %s", enc)
}

func parseBody(raw []byte, contentType string) (interface{}, string) {
	if len(raw) == 0 {
		return nil, "empty"
	}
	ctype := strings.Split(contentType, ";")[0]
	ctype = strings.ToLower(strings.TrimSpace(ctype))

	if strings.Contains(ctype, "protobuf") {
		limit := len(raw)
		if limit > 64 {
			limit = 64
		}
		return map[string]interface{}{
			"_base64": base64.StdEncoding.EncodeToString(raw),
			"_hex":    hex.EncodeToString(raw[:limit]),
		}, "protobuf-raw"
	}

	var jsonVal interface{}
	if err := json.Unmarshal(raw, &jsonVal); err == nil {
		return jsonVal, "json"
	}

	text := string(raw)
	if strings.Contains(ctype, "ndjson") || (strings.Count(text, "\n") > 0 && (strings.HasPrefix(strings.TrimSpace(text), "{") || strings.HasPrefix(strings.TrimSpace(text), "["))) {
		lines := strings.Split(text, "\n")
		var ndList []interface{}
		validND := true
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var item interface{}
			if err := json.Unmarshal([]byte(line), &item); err != nil {
				validND = false
				break
			}
			ndList = append(ndList, item)
		}
		if validND && len(ndList) > 0 {
			return ndList, "ndjson"
		}
	}

	if isBinary(raw) {
		return map[string]interface{}{
			"_base64": base64.StdEncoding.EncodeToString(raw),
		}, "binary"
	}

	return text, "text"
}

func isBinary(data []byte) bool {
	for _, b := range data {
		if b == 0 {
			return true
		}
	}
	return false
}

func safeName(path string) string {
	cleaned := strings.ReplaceAll(strings.Trim(path, "/"), "/", "_")
	if cleaned == "" {
		cleaned = "root"
	}
	var sb strings.Builder
	for _, r := range cleaned {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			sb.WriteRune(r)
		} else {
			sb.WriteRune('-')
		}
	}
	res := sb.String()
	if len(res) > 80 {
		return res[:80]
	}
	return res
}

func catchAllHandler(w http.ResponseWriter, r *http.Request) {
	received := getNow()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		raw = []byte{}
	}

	body, decodeErr := decompressBody(raw, r.Header.Get("Content-Encoding"))
	contentType := r.Header.Get("Content-Type")
	parsedBody, bodyFormat := parseBody(body, contentType)

	if state.Config.MaxBody > 0 {
		if str, ok := parsedBody.(string); ok && len(str) > state.Config.MaxBody {
			parsedBody = str[:state.Config.MaxBody] + fmt.Sprintf("... [truncated, %d chars]", len(str))
		}
	}

	// Atomic lock-free sequence increment
	seq := atomic.AddInt64(&state.Seq, 1)

	signal := otlpSignal(r.URL.Path)

	// Set CORS headers
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "*")
	w.Header().Set("Access-Control-Allow-Headers", "*")
	w.Header().Set("Access-Control-Expose-Headers", "*")

	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	headersDict := make(map[string]string)
	for k, v := range r.Header {
		headersDict[strings.ToLower(k)] = strings.Join(v, ", ")
	}

	queryParams := make(map[string]string)
	for k, v := range r.URL.Query() {
		queryParams[k] = strings.Join(v, ", ")
	}

	var status int
	var proxyTargetURL string
	var elapsedMs float64
	var proxyErrStr string
	var proxyRespHeaders http.Header
	var proxyRespBody []byte

	isProxy := state.Config.ProxyTarget != ""

	if isProxy {
		targetBase := state.Config.ProxyTarget
		pathPart := "/" + strings.TrimLeft(r.URL.Path, "/")
		queryPart := ""
		if r.URL.RawQuery != "" {
			queryPart = "?" + r.URL.RawQuery
		}
		proxyTargetURL = targetBase + pathPart + queryPart

		outReq, err := http.NewRequest(r.Method, proxyTargetURL, bytes.NewReader(raw))
		if err != nil {
			proxyErrStr = fmt.Sprintf("failed to create request: %v", err)
			status = 502
		} else {
			for k, v := range r.Header {
				lk := strings.ToLower(k)
				if lk == "host" || lk == "content-length" || lk == "transfer-encoding" || lk == "connection" {
					continue
				}
				for _, val := range v {
					outReq.Header.Add(k, val)
				}
			}

			startTime := time.Now()
			resp, err := sharedHTTPClient.Do(outReq)
			elapsedMs = float64(time.Since(startTime).Microseconds()) / 1000.0

			if err != nil {
				proxyErrStr = fmt.Sprintf("proxy request failed: %v", err)
				status = 502
				proxyRespBody, _ = json.Marshal(map[string]string{"error": "Bad Gateway", "detail": err.Error()})
			} else {
				defer resp.Body.Close()
				status = resp.StatusCode
				proxyRespHeaders = resp.Header
				proxyRespBody, _ = io.ReadAll(resp.Body)
			}
		}

		if state.Config.Status > 0 {
			status = state.Config.Status
		}
	} else {
		if state.Config.Status > 0 {
			status = state.Config.Status
		} else {
			status = 200
		}
	}

	record := map[string]interface{}{
		"seq":             seq,
		"timestamp":       received.Format(time.RFC3339),
		"method":          r.Method,
		"url":             r.URL.String(),
		"path":            r.URL.Path,
		"client":          r.RemoteAddr,
		"http_version":    fmt.Sprintf("HTTP/%d.%d", r.ProtoMajor, r.ProtoMinor),
		"otlp_signal":     signal,
		"body_format":     bodyFormat,
		"body_bytes":      len(raw),
		"decoded_bytes":   len(body),
		"decode_error":    decodeErr,
		"headers":         headersDict,
		"query_params":    queryParams,
		"body":            parsedBody,
		"response_status": status,
	}

	if isProxy {
		record["proxy_target"] = proxyTargetURL
		record["upstream_latency_ms"] = elapsedMs
		if proxyErrStr != "" {
			record["proxy_error"] = proxyErrStr
		}
	}

	filename := fmt.Sprintf("%05d_%s_%s.json", seq, r.Method, safeName(r.URL.Path))
	
	// Non-blocking concurrent disk write in background goroutine
	go writeRecord(record, filename)

	if isProxy {
		fmt.Printf("#%05d %s %s <- %s %dB %s => %s -> %d (%.1fms)\n", seq, r.Method, r.URL.Path, r.RemoteAddr, len(raw), bodyFormat, proxyTargetURL, status, elapsedMs)
	} else {
		fmt.Printf("#%05d %s %s <- %s %dB %s -> %d\n", seq, r.Method, r.URL.Path, r.RemoteAddr, len(raw), bodyFormat, status)
	}

	if decodeErr != "" {
		fmt.Printf("  ! %s\n", decodeErr)
	}
	if proxyErrStr != "" {
		fmt.Printf("  ! %s\n", proxyErrStr)
	}

	if state.Config.Delay > 0 {
		time.Sleep(time.Duration(state.Config.Delay * float64(time.Second)))
	}

	if isProxy {
		if proxyRespHeaders != nil {
			for k, v := range proxyRespHeaders {
				lk := strings.ToLower(k)
				if lk == "content-encoding" || lk == "content-length" || lk == "transfer-encoding" || lk == "connection" {
					continue
				}
				for _, val := range v {
					w.Header().Add(k, val)
				}
			}
		}
		w.WriteHeader(status)
		w.Write(proxyRespBody)
		return
	}

	respondMock(w, r, signal, contentType, status, record)
}

func respondMock(w http.ResponseWriter, r *http.Request, signal, contentType string, status int, record map[string]interface{}) {
	if r.Method == "HEAD" || r.Method == "OPTIONS" {
		if status == 200 {
			w.WriteHeader(http.StatusNoContent)
		} else {
			w.WriteHeader(status)
		}
		return
	}

	if signal != "" {
		ctype := strings.Split(contentType, ";")[0]
		if strings.Contains(strings.ToLower(ctype), "protobuf") {
			w.Header().Set("Content-Type", "application/x-protobuf")
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte("{}"))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if r.Method == "GET" {
		data, _ := json.Marshal(record)
		w.Write(data)
		return
	}

	respMap := map[string]interface{}{"ok": status < 400, "seq": record["seq"]}
	data, _ := json.Marshal(respMap)
	w.Write(data)
}

func writeRecord(record map[string]interface{}, filename string) {
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return
	}
	filePath := filepath.Join(state.SessionDir, filename)
	os.WriteFile(filePath, data, 0644)

	indexLine := map[string]interface{}{
		"seq":    record["seq"],
		"ts":     record["timestamp"],
		"method": record["method"],
		"path":   record["path"],
		"status": record["response_status"],
		"bytes":  record["body_bytes"],
		"file":   filename,
	}
	indexData, _ := json.Marshal(indexLine)

	state.IndexMutex.Lock()
	defer state.IndexMutex.Unlock()

	f, err := os.OpenFile(state.IndexPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err == nil {
		defer f.Close()
		f.Write(append(indexData, '\n'))
	}
}
