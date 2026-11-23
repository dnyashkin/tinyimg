package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math/rand"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// testPNG — градиент с шумом: больше 256 цветов, сжимается в палитру.
// Шум обязателен: чистый градиент сам по себе жмётся в килобайт,
// и палитровая стратегия его не обгонит (см. compress_test.go).
func testPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 128, 128))
	rng := rand.New(rand.NewSource(1))
	for y := 0; y < 128; y++ {
		for x := 0; x < 128; x++ {
			n := rng.Intn(48) - 24
			clamp := func(v int) uint8 {
				if v < 0 {
					return 0
				}
				if v > 255 {
					return 255
				}
				return uint8(v)
			}
			img.SetNRGBA(x, y, color.NRGBA{
				R: clamp(x*2 + n), G: clamp(y*2 + n), B: clamp(x + y + n), A: 255,
			})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func upload(t *testing.T, ts *httptest.Server, field, name string, data []byte) compressResponse {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile(field, name)
	if err != nil {
		t.Fatal(err)
	}
	fw.Write(data)
	mw.Close()

	resp, err := http.Post(ts.URL+"/api/compress", mw.FormDataContentType(), &body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус %d", resp.StatusCode)
	}
	var out compressResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestCompressDownloadZip — полный цикл: загрузка → скачивание → zip.
func TestCompressDownloadZip(t *testing.T) {
	ts := httptest.NewServer(newMux(nil))
	defer ts.Close()

	pngData := testPNG(t)
	r1 := upload(t, ts, "file", "фото.png", pngData)
	if !r1.OK || r1.File == nil {
		t.Fatalf("ответ: %+v", r1)
	}
	if r1.File.After >= r1.File.Before {
		t.Fatalf("не сжалось: %d -> %d", r1.File.Before, r1.File.After)
	}
	r2 := upload(t, ts, "file", "второе.png", pngData)
	if !r2.OK {
		t.Fatalf("ответ 2: %+v", r2)
	}

	// Скачивание одного файла.
	resp, err := http.Get(ts.URL + "/api/file/" + r1.File.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if len(got) != int(r1.File.After) {
		t.Fatalf("скачали %d байт, ожидалось %d", len(got), r1.File.After)
	}
	if cd := resp.Header.Get("Content-Disposition"); cd == "" {
		t.Fatal("нет Content-Disposition")
	}

	// Zip с обоими.
	resp2, err := http.Get(ts.URL + "/api/zip?ids=" +
		url.QueryEscape(r1.File.ID+","+r2.File.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	data, _ := io.ReadAll(resp2.Body)
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("zip не читается: %v", err)
	}
	if len(zr.File) != 2 {
		t.Fatalf("в zip %d файлов, ожидалось 2", len(zr.File))
	}
}

// TestRejectsNonImage — не-картинки отвергаются.
func TestRejectsNonImage(t *testing.T) {
	ts := httptest.NewServer(newMux(nil))
	defer ts.Close()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "notes.txt")
	fw.Write([]byte("hello"))
	mw.Close()
	resp, err := http.Post(ts.URL+"/api/compress", mw.FormDataContentType(), &body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("статус %d, ожидался 400", resp.StatusCode)
	}
}

// TestManifestMimeType — манифест отдаётся с application/manifest+json.
// Go не знает .webmanifest, и без явного Content-Type браузер отвергнет
// манифест молча (PWA-иконки просто не появятся).
func TestManifestMimeType(t *testing.T) {
	ts := httptest.NewServer(newMux(nil))
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/site.webmanifest")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус %d, ожидался 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/manifest+json; charset=utf-8" {
		t.Fatalf("Content-Type манифеста: %q", ct)
	}
	data, _ := io.ReadAll(resp.Body)
	var m struct {
		Name string `json:"name"`
		Icons []struct {
			Src string `json:"src"`
		} `json:"icons"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("манифест не парсится: %v", err)
	}
	if m.Name == "" || len(m.Icons) == 0 {
		t.Fatalf("манифест пустой: %+v", m)
	}
}

// TestStaticAssetsServed — favicon/robots/sitemap/og-image отдаются
// и лежат по путям, на которые ссылается index.html.
func TestStaticAssetsServed(t *testing.T) {
	ts := httptest.NewServer(newMux(nil))
	defer ts.Close()
	for _, path := range []string{
		"/favicon.ico",
		"/img/favicon-32x32.png",
		"/img/favicon-16x16.png",
		"/img/apple-touch-icon-152x152.png",
		"/img/og-image.png",
		"/robots.txt",
		"/sitemap.xml",
	} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: статус %d", path, resp.StatusCode)
		}
	}
}

// TestIndexServed — главная отдаётся из embed.
func TestIndexServed(t *testing.T) {
	ts := httptest.NewServer(newMux(nil))
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(data, []byte("tinyimg")) {
		t.Fatalf("index.html не тот: %q", data[:80])
	}
}

// TestUrlEncode — кириллица кодируется байтами с двумя hex-цифрами.
func TestUrlEncode(t *testing.T) {
	got := urlEncode("фото.png")
	want := "%D1%84%D0%BE%D1%82%D0%BE.png"
	if got != want {
		t.Fatalf("urlEncode: %s != %s", got, want)
	}
	fmt.Println("ok:", got)
}
