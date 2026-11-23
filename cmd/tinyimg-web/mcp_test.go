package main

// Тесты лимитера по IP и MCP-эндпоинта.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// postIP — запрос с подделанным X-Forwarded-For (обычный клиент так может).
func postIP(ts *httptest.Server, xff string) *http.Response {
	req, _ := http.NewRequest("POST", ts.URL, bytes.NewReader(nil))
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		panic(err)
	}
	return resp
}

func TestIPLimiterBurst(t *testing.T) {
	l := newIPLimiter(60, 2, 1) // 60/мин, burst 2, одно одновременное
	s := newServer(l)
	ts := httptest.NewServer(http.HandlerFunc(s.handleCompress))
	defer ts.Close()

	// burst=2 → два запроса проходят (форма будет битой, но лимитер раньше),
	// третий должен получить 429.
	var codes []int
	for i := 0; i < 3; i++ {
		resp := postIP(ts, "")
		codes = append(codes, resp.StatusCode)
		resp.Body.Close()
	}
	if codes[0] != http.StatusBadRequest || codes[1] != http.StatusBadRequest || codes[2] != http.StatusTooManyRequests {
		t.Fatalf("ожидалось 400, 400, 429 — получили %v", codes)
	}
}

// TestIPLimiterIgnoresSpoofedXFF — подмена X-Forwarded-For не должна обходить
// лимит: заголовок учитывается только за доверенным прокси.
func TestIPLimiterIgnoresSpoofedXFF(t *testing.T) {
	l := newIPLimiter(60, 2, 1)
	s := newServer(l)
	ts := httptest.NewServer(http.HandlerFunc(s.handleCompress))
	defer ts.Close()

	for i, xff := range []string{"203.0.113.7", "203.0.113.8", "203.0.113.9"} {
		resp := postIP(ts, xff)
		code := resp.StatusCode
		resp.Body.Close()
		want := http.StatusBadRequest
		if i == 2 {
			want = http.StatusTooManyRequests
		}
		if code != want {
			t.Fatalf("запрос %d с XFF %s: ожидалось %d, получили %d", i, xff, want, code)
		}
	}
}

// TestIPLimiterTrustedProxy — за доверенным прокси X-Forwarded-For
// учитывается, и разные клиенты получают свои корзины.
func TestIPLimiterTrustedProxy(t *testing.T) {
	_, proxyNet, err := net.ParseCIDR("127.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	l := newIPLimiter(60, 2, 1)
	s := newServer(l, proxyNet)
	ts := httptest.NewServer(http.HandlerFunc(s.handleCompress))
	defer ts.Close()

	// Первый клиент исчерпывает свой burst.
	for i := 0; i < 3; i++ {
		resp := postIP(ts, "203.0.113.7")
		code := resp.StatusCode
		resp.Body.Close()
		if i < 2 && code != http.StatusBadRequest {
			t.Fatalf("запрос %d: ожидалось 400, получили %d", i, code)
		}
		if i == 2 && code != http.StatusTooManyRequests {
			t.Fatalf("третий запрос: ожидалось 429, получили %d", code)
		}
	}
	// Второй клиент (другой XFF) — своя корзина, проходит.
	resp := postIP(ts, "203.0.113.8")
	code := resp.StatusCode
	resp.Body.Close()
	if code != http.StatusBadRequest {
		t.Fatalf("чужой IP за доверенным прокси должен проходить, получили %d", code)
	}
}

// TestRetryAfterHeader — 429 содержит честный Retry-After (не «1» при rate 6).
func TestRetryAfterHeader(t *testing.T) {
	l := newIPLimiter(6, 1, 1) // 6/мин → ожидание до 10 секунд
	s := newServer(l)
	ts := httptest.NewServer(http.HandlerFunc(s.handleCompress))
	defer ts.Close()

	for i := 0; i < 2; i++ {
		resp := postIP(ts, "")
		code := resp.StatusCode
		ra := resp.Header.Get("Retry-After")
		resp.Body.Close()
		if i == 0 {
			continue
		}
		if code != http.StatusTooManyRequests {
			t.Fatalf("ожидалось 429, получили %d", code)
		}
		n, err := strconv.Atoi(ra)
		if err != nil || n < 1 {
			t.Fatalf("Retry-After: ожидалось целое >= 1, получили %q", ra)
		}
		if n < 5 {
			t.Fatalf("Retry-After должен отражать реальное ожидание (rate 6/мин → ~10 c), получили %q", ra)
		}
	}
}

func TestMCPFlow(t *testing.T) {
	ts := httptest.NewServer(newMux(nil))
	defer ts.Close()

	call := func(body map[string]any) map[string]any {
		b, _ := json.Marshal(body)
		resp, err := http.Post(ts.URL+"/mcp", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	// initialize
	init := call(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{}})
	if _, ok := init["result"]; !ok {
		t.Fatalf("initialize без result: %v", init)
	}
	if v, _ := init["result"].(map[string]any)["protocolVersion"].(string); v != "2025-03-26" {
		t.Fatalf("неожиданная версия протокола: %q", v)
	}

	// tools/list
	list := call(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	res := list["result"].(map[string]any)
	tools := res["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "compress_image" {
		t.Fatalf("неожиданные тулзы: %v", tools)
	}

	// tools/call: маленький шумный PNG
	img := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	for i := range img.Pix {
		img.Pix[i] = uint8(i%251 + 3)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	callRes := call(map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{
			"name": "compress_image",
			"arguments": map[string]any{
				"image": base64.StdEncoding.EncodeToString(buf.Bytes()),
				"name":  "test.png",
			},
		},
	})
	result := callRes["result"].(map[string]any)
	if result["isError"].(bool) {
		t.Fatalf("tools/call ошибка: %v", result)
	}
	content := result["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("ожидалось 2 элемента контента, %d", len(content))
	}
	imgItem := content[1].(map[string]any)
	if imgItem["type"] != "image" || imgItem["mimeType"] != "image/png" {
		t.Fatalf("неожиданный image-контент: %v", imgItem)
	}
	if _, err := base64.StdEncoding.DecodeString(imgItem["data"].(string)); err != nil {
		t.Fatalf("data не base64: %v", err)
	}
	// В summary — имя файла, которое задал вызывающий (не временный файл).
	summary := content[0].(map[string]any)["text"].(string)
	if !bytes.Contains([]byte(summary), []byte("test.png")) {
		t.Fatalf("в summary ожидалось имя test.png: %s", summary)
	}

	// неизвестный метод → JSON-RPC ошибка
	unk := call(map[string]any{"jsonrpc": "2.0", "id": 4, "method": "nope"})
	if unk["error"] == nil {
		t.Fatalf("неизвестный метод без ошибки: %v", unk)
	}
	// id в ответе присутствует (JSON-RPC 2.0)
	if unk["id"] == nil {
		t.Fatalf("в ответе нет id: %v", unk)
	}
}

// TestMCPNotification — запрос без id это нотификация: 202 без тела.
func TestMCPNotification(t *testing.T) {
	ts := httptest.NewServer(newMux(nil))
	defer ts.Close()
	body := []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	resp, err := http.Post(ts.URL+"/mcp", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("нотификация: ожидалось 202, получили %d", resp.StatusCode)
	}
	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	if buf.Len() != 0 {
		t.Fatalf("нотификация: ожидалось пустое тело, получили %q", buf.String())
	}
}

// TestMCPRateLimitJSONRPC — 429 на MCP ответе приходит JSON-RPC-ошибкой,
// а не site-payload: клиент должен показать сообщение, а не ошибку парсинга.
func TestMCPRateLimitJSONRPC(t *testing.T) {
	l := newIPLimiter(60, 1, 1)
	ts := httptest.NewServer(newMux(l))
	defer ts.Close()

	send := func() (*http.Response, map[string]any) {
		body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
		resp, err := http.Post(ts.URL+"/mcp", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		return resp, out
	}

	resp, first := send()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("первый запрос: ожидалось 200, получили %d", resp.StatusCode)
	}
	if first["result"] == nil {
		t.Fatalf("первый запрос без result: %v", first)
	}
	resp2, second := send()
	if resp2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("второй запрос: ожидалось 429, получили %d", resp2.StatusCode)
	}
	if second["jsonrpc"] != "2.0" {
		t.Fatalf("ответ 429 должен быть JSON-RPC-обёрткой: %v", second)
	}
	if second["error"] == nil {
		t.Fatalf("ответ 429 без error: %v", second)
	}
	if resp2.Header.Get("Retry-After") == "" {
		t.Fatalf("ответ 429 без Retry-After")
	}
}

// TestMCPGetInfo — GET /mcp отдаёт понятную страницу (а не 404).
func TestMCPGetInfo(t *testing.T) {
	ts := httptest.NewServer(newMux(nil))
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/mcp")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /mcp: ожидалось 200, получили %d", resp.StatusCode)
	}
	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	if !bytes.Contains(buf.Bytes(), []byte("POST /mcp")) {
		t.Fatalf("страница /mcp без описания эндпоинта: %q", buf.String())
	}
}
