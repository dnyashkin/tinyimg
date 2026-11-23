package main

// MCP-сервер (Streamable HTTP, stateless): POST /mcp принимает JSON-RPC 2.0
// и отдаёт JSON-ответы. Один тулз — compress_image: те же сжатия, что на
// сайте (PNG/JPEG/WebP через internal/compress), base64 в обе стороны.
// Лимитер тот же, что на /api/compress: агенты не должны валить сервис.

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"tinyimg/internal/compress"
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

var version = "0.1.0" // для serverInfo; перекрывается -ldflags

func rpcErr(id json.RawMessage, code int, msg string) rpcResponse {
	if len(id) == 0 {
		id = json.RawMessage("null") // id не определён → null по JSON-RPC 2.0
	}
	return rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}

func (s *server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "MCP принимает только POST (stateless streamable HTTP)", http.StatusMethodNotAllowed)
		return
	}
	ip := ipFromRequest(r, s.proxies)
	if ok, retry := s.limiter.allow(ip); !ok {
		// 429 в JSON-RPC-обёртке: MCP-клиент ждёт JSON-RPC, а не наш
		// site-payload — иначе он покажет ошибку парсинга вместо «подождите».
		w.Header().Set("Retry-After", fmt.Sprintf("%d", retryAfterSecs(retry)))
		writeMCPStatus(w, http.StatusTooManyRequests, rpcErr(nil, -32000, "слишком много запросов, попробуйте позже"))
		return
	}

	// Глобальный предел параллелизма — ДО чтения тела: base64-тело до
	// ~134 МБ живёт целиком в памяти (JSON + строка + декод), и без
	// семафора N соединений умножали бы это безгранично. Слот держим весь
	// запрос (handleToolCall его не берёт повторно — иначе дедлок).
	s.jobs <- struct{}{}
	defer func() { <-s.jobs }()

	var req rpcRequest
	// base64 раздувает файл ровно в 4/3, плюс JSON-накладные.
	r.Body = http.MaxBytesReader(w, r.Body, maxFileSize/3*4+1<<20)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeMCP(w, rpcErr(nil, -32600, "запрос слишком большой: максимум 100 МБ файла"))
			return
		}
		writeMCP(w, rpcErr(nil, -32700, "битый JSON"))
		return
	}
	if req.JSONRPC != "2.0" {
		writeMCP(w, rpcErr(req.ID, -32600, "ожидается jsonrpc: \"2.0\""))
		return
	}
	// Запрос без id — это нотификация: по Streamable HTTP отвечаем 202 без тела.
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	switch req.Method {
	case "initialize":
		writeMCP(w, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "tinyimg", "version": version},
			"instructions":    "Сжатие PNG/JPEG/WebP: загрузите изображение в base64, получите сжатое обратно.",
		}})
	case "notifications/initialized", "notifications/cancelled":
		w.WriteHeader(http.StatusAccepted) // нотификации без ответа
	case "ping":
		writeMCP(w, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}})
	case "tools/list":
		writeMCP(w, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
			"tools": []any{map[string]any{
				"name":        "compress_image",
				"description": "Сжать PNG/JPEG/WebP без заметной потери качества (как на сайте tinyimg). Вход — base64, выход — сжатое изображение base64 + экономия.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"image": map[string]any{"type": "string", "description": "Изображение в base64 (PNG, JPEG или WebP)"},
						"name":  map[string]any{"type": "string", "description": "Имя файла с расширением — для подписи результата (опционально; формат определяется по содержимому)"},
					},
					"required": []string{"image"},
				},
			}},
		}})
	case "tools/call":
		s.handleToolCall(w, r, req)
	default:
		writeMCP(w, rpcErr(req.ID, -32601, "неизвестный метод: "+req.Method))
	}
}

// handleMCPInfo — понятная страница для людей, открывших /mcp в браузере.
// Протокол MCP работает только через POST (JSON-RPC 2.0).
func (s *server) handleMCPInfo(w http.ResponseWriter, r *http.Request) {
	base := s.schemeOf(r) + "://" + r.Host
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprint(w, `tinyimg — MCP-сервер (Model Context Protocol)

Эндпоинт: POST /mcp (Streamable HTTP, stateless, JSON-RPC 2.0)
Версия протокола: 2025-03-26

Подключение в MCP-клиенте (Claude Desktop, Cursor, Windsurf…):
  {"mcpServers": {"tinyimg": {"url": "`+base+`/mcp"}}}

Тулзы:
  compress_image — сжатие PNG/JPEG/WebP (base64 вход и выход)

Пример запроса:
  curl -X POST `+base+`/mcp -H 'Content-Type: application/json' \
    -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'

Удалённым клиентам нужен https:// (http:// — только для локального запуска).
`)
}

// schemeOf — схема запроса с учётом обратного прокси. X-Forwarded-Proto
// подделывается любым клиентом, поэтому учитываем его только когда
// непосредственный пир входит в -trust-proxy (та же логика, что у XFF).
func (s *server) schemeOf(r *http.Request) string {
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" && trustedPeer(r, s.proxies) {
		return strings.TrimSpace(strings.Split(p, ",")[0])
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// trustedPeer — непосредственный клиент соединения входит в список
// доверенных прокси.
func trustedPeer(r *http.Request, trusted []*net.IPNet) bool {
	if len(trusted) == 0 {
		return false
	}
	pip := net.ParseIP(peerIP(r))
	if pip == nil {
		return false
	}
	for _, n := range trusted {
		if n != nil && n.Contains(pip) {
			return true
		}
	}
	return false
}

func writeMCP(w http.ResponseWriter, resp rpcResponse) {
	writeMCPStatus(w, http.StatusOK, resp)
}

func writeMCPStatus(w http.ResponseWriter, status int, resp rpcResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(resp)
}

type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func (s *server) handleToolCall(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	var p toolCallParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		writeMCP(w, rpcErr(req.ID, -32602, "битые params: "+err.Error()))
		return
	}
	if p.Name != "compress_image" {
		writeMCP(w, rpcErr(req.ID, -32602, "неизвестный тулз: "+p.Name))
		return
	}
	var args struct {
		Image string `json:"image"`
		Name  string `json:"name"`
	}
	if err := json.Unmarshal(p.Arguments, &args); err != nil || args.Image == "" {
		writeMCP(w, rpcErr(req.ID, -32602, "ожидается arguments.image (base64)"))
		return
	}

	raw, ok := decodeBase64(args.Image)
	if !ok {
		toolErr(w, req.ID, "image не декодируется как base64")
		return
	}
	if int64(len(raw)) > maxFileSize {
		toolErr(w, req.ID, "слишком большой файл (лимит 100 МБ)")
		return
	}

	ext := imageExt(raw)
	if ext == "" {
		toolErr(w, req.ID, "не распознан формат: нужен PNG, JPEG или WebP")
		return
	}

	// Приватный каталог (0700): результат пишется с правами 0644, в общем
	// /tmp его читал бы любой пользователь машины.
	dir, err := os.MkdirTemp("", "tinyimg-mcp-*")
	if err != nil {
		toolErr(w, req.ID, "внутренняя ошибка: "+err.Error())
		return
	}
	defer os.RemoveAll(dir)
	src := filepath.Join(dir, "src"+ext)
	dst := filepath.Join(dir, "out"+ext)
	if err := os.WriteFile(src, raw, 0o600); err != nil {
		toolErr(w, req.ID, "внутренняя ошибка: "+err.Error())
		return
	}

	// Глобальный слот параллелизма уже держит handleMCP (с момента чтения
	// тела) — повторно не берём: при 4 занятых слотах был бы дедлок.
	// Остаётся per-IP слот на само сжатие.
	ip := ipFromRequest(r, s.proxies)
	if !s.limiter.acquire(ip) {
		toolErr(w, req.ID, "слишком много одновременных сжатий, попробуйте позже")
		return
	}
	defer s.limiter.release(ip)

	opts := compress.Options{Dither: -1}
	if len(raw) < 500<<10 {
		opts.Squeeze = true // как на сайте: мелкие пережимаем плотнее
	}
	res := compress.File(src, dst, opts)
	if res.Err != nil {
		toolErr(w, req.ID, "сжатие не удалось: "+res.Err.Error())
		return
	}
	out, err := os.ReadFile(dst)
	if err != nil {
		toolErr(w, req.ID, "внутренняя ошибка: "+err.Error())
		return
	}

	mimeType := map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".webp": "image/webp"}[ext]
	name := args.Name
	if name == "" {
		name = res.Name
	}
	summary, _ := json.Marshal(map[string]any{
		"name":     name,
		"before":   res.Before,
		"after":    res.After,
		"saved":    res.SavedPercent(),
		"strategy": res.Strategy,
	})
	writeMCP(w, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
		"content": []any{
			map[string]any{"type": "text", "text": string(summary)},
			map[string]any{"type": "image", "data": base64.StdEncoding.EncodeToString(out), "mimeType": mimeType},
		},
		"isError": false,
	}})
}

// toolErr — ошибки уровня тулза по спецификации MCP (result.isError),
// а не JSON-RPC-ошибки: клиент должен показать текст пользователю.
func toolErr(w http.ResponseWriter, id json.RawMessage, msg string) {
	writeMCP(w, rpcResponse{JSONRPC: "2.0", ID: id, Result: map[string]any{
		"content": []any{map[string]any{"type": "text", "text": msg}},
		"isError": true,
	}})
}

// decodeBase64 — терпит пробелы и переносы (щедрые клиенты их вставляют) и
// паддинг. Ошибка второй попытки не игнорируется: частично декодированные
// данные не должны уходить в кодек.
func decodeBase64(s string) ([]byte, bool) {
	clean := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\r':
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if clean == "" {
		return nil, false
	}
	if raw, err := base64.StdEncoding.DecodeString(clean); err == nil {
		return raw, len(raw) > 0
	}
	raw, err := base64.RawStdEncoding.DecodeString(clean)
	if err != nil {
		return nil, false
	}
	return raw, len(raw) > 0
}

// imageExt — формат по магии байтов. Имя файла — только подсказка: без magic-
// байтов формат не определить, и кодировщик всё равно не сможет декодировать
// такой файл — так что подсказка лишь уточняет текст ошибки.
func imageExt(data []byte) string {
	switch {
	case len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n":
		return ".png"
	case len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF:
		return ".jpg"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return ".webp"
	}
	return ""
}
