package main

// Тесты защит веб-сервиса: дедуп/метод zip, бюджет хранилища, IPv6-ключ
// лимитера, fail-closed потолок карты корзин, гард размеров на HTTP-уровне.

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestZipDeduplicatesIDs — повторные id не умножают поток, метод Store.
func TestZipDeduplicatesIDs(t *testing.T) {
	ts := httptest.NewServer(newMux(nil))
	defer ts.Close()

	r1 := upload(t, ts, "file", "один.png", testPNG(t))
	if !r1.OK {
		t.Fatalf("ответ: %+v", r1)
	}
	ids := r1.File.ID + "," + r1.File.ID + "," + r1.File.ID
	resp, err := http.Get(ts.URL + "/api/zip?ids=" + url.QueryEscape(ids))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("zip не читается: %v", err)
	}
	if len(zr.File) != 1 {
		t.Fatalf("дубликаты id не отрезаны: %d файлов в zip", len(zr.File))
	}
	if zr.File[0].Method != zip.Store {
		t.Fatalf("ожидался Store (картинки уже сжаты), метод %d", zr.File[0].Method)
	}
}

// TestZipRateLimited — /api/zip под тем же лимитером, что и сжатие.
func TestZipRateLimited(t *testing.T) {
	l := newIPLimiter(60, 1, 1)
	s := newServer(l)
	t.Cleanup(s.close)
	ts := httptest.NewServer(http.HandlerFunc(s.handleZip))
	defer ts.Close()

	for i, want := range []int{http.StatusNotFound, http.StatusTooManyRequests} {
		resp, err := http.Get(ts.URL + "/api/zip?ids=nope")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("запрос %d: ожидалось %d, получили %d", i, want, resp.StatusCode)
		}
	}
}

// TestStoreBudgetEvictsOldest — при переполнении бюджета памяти вытесняется
// самый старый результат, свежие остаются доступны.
func TestStoreBudgetEvictsOldest(t *testing.T) {
	s := newServer(nil)
	t.Cleanup(s.close)
	s.maxStore = 100

	mk := func(id string, size int, created time.Time) *result {
		return &result{ID: id, Name: id + ".png", Data: make([]byte, size), Created: created}
	}
	s.put(mk("old", 80, time.Now().Add(-time.Minute)))
	s.put(mk("new", 80, time.Now())) // 160 > 100 → «old» вытесняется

	if s.get("old") != nil {
		t.Fatal("старый результат не вытеснен при переполнении бюджета")
	}
	if s.get("new") == nil {
		t.Fatal("свежий результат потерян")
	}
	if s.storeBytes != 80 {
		t.Fatalf("storeBytes = %d, ожидалось 80", s.storeBytes)
	}
}

// TestIPv6SharedBucket — адреса одного /64 делят корзину: поточечный ключ
// позволял обходить лимит перебором адресов подсети.
func TestIPv6SharedBucket(t *testing.T) {
	l := newIPLimiter(60, 2, 0)
	for _, ip := range []string{"2001:db8::1", "2001:db8::2"} {
		if ok, _ := l.allow(ip); !ok {
			t.Fatalf("%s: первые два запроса /64 должны проходить", ip)
		}
	}
	// Третий адрес той же /64 — третий токен, а burst исчерпан.
	if ok, _ := l.allow("2001:db8::ffff"); ok {
		t.Fatal("адрес из той же /64 получил отдельную корзину — обход лимита")
	}
	// Другая /64 — своя корзина.
	if ok, _ := l.allow("2001:db9::1"); !ok {
		t.Fatal("другая /64 должна получать свою корзину")
	}
	// IPv4 — поточечно, как раньше.
	if ok, _ := l.allow("203.0.113.1"); !ok {
		t.Fatal("IPv4 должен проходить")
	}
	if ok, _ := l.allow("203.0.113.2"); !ok {
		t.Fatal("другой IPv4 должен получать свою корзину")
	}
}

// TestLimiterFailClosedAtCap — за потолком карты новые ключи отклоняются,
// а существующие продолжают работать (без полного сброса лимитов).
func TestLimiterFailClosedAtCap(t *testing.T) {
	l := newIPLimiter(60, 1, 0)
	l.buckets["regular"] = &ipBucket{tokens: 1, last: time.Now(), lastSeen: time.Now()}
	for i := 0; len(l.buckets) < maxBuckets; i++ {
		l.buckets[fmt.Sprintf("flood-%d", i)] = &ipBucket{last: time.Now(), lastSeen: time.Now()}
	}
	if ok, _ := l.allow("brand-new-ip"); ok {
		t.Fatal("новый ключ за потолком карты должен отклоняться")
	}
	if ok, _ := l.allow("regular"); !ok {
		t.Fatal("существующая корзина должна продолжать работать")
	}
	if ok, _ := l.allow("regular"); ok {
		t.Fatal("токен существующей корзины уже потрачен — должно быть 429")
	}
}

// hugePNG — минимальный PNG с огромными размерами в IHDR (сигнатура +
// IHDR + IEND): для срабатывания гарда декод не нужен.
func hugePNG(w, h uint32) []byte {
	var out bytes.Buffer
	out.Write([]byte{137, 80, 78, 71, 13, 10, 26, 10})
	writeChunk := func(typ string, data []byte) {
		var hdr [8]byte
		binary.BigEndian.PutUint32(hdr[:4], uint32(len(data)))
		copy(hdr[4:], typ)
		out.Write(hdr[:])
		out.Write(data)
		crc := crc32.NewIEEE()
		crc.Write(hdr[4:])
		crc.Write(data)
		var c [4]byte
		binary.BigEndian.PutUint32(c[:], crc.Sum32())
		out.Write(c[:])
	}
	var ihdr [13]byte
	binary.BigEndian.PutUint32(ihdr[0:4], w)
	binary.BigEndian.PutUint32(ihdr[4:8], h)
	ihdr[8], ihdr[9] = 8, 6 // 8 бит, RGBA
	writeChunk("IHDR", ihdr[:])
	writeChunk("IEND", nil)
	return out.Bytes()
}

// TestUploadTooLargePNG — гард размеров на HTTP-уровне: загрузка «бомбы»
// возвращает ошибку, а не кладёт сервер по памяти.
func TestUploadTooLargePNG(t *testing.T) {
	ts := httptest.NewServer(newMux(nil))
	defer ts.Close()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "bomb.png")
	if err != nil {
		t.Fatal(err)
	}
	fw.Write(hugePNG(12000, 12000))
	mw.Close()

	resp, err := http.Post(ts.URL+"/api/compress", mw.FormDataContentType(), &body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("ожидался 500 на несжимаемой «бомбе», получили %d", resp.StatusCode)
	}
	var out compressResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Error, "не удалось сжать") {
		t.Fatalf("неожиданный текст ошибки: %+v", out)
	}
}
