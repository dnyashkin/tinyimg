// tinyimg-web — веб-интерфейс компрессора PNG/JPEG (аналог TinyPNG).
//
// Инструмент на первом экране: drag&drop до 20 картинок, сжатие через
// internal/compress (то же, что CLI), скачивание по одному или zip-архивом.
// Файлы живут в памяти сервера и удаляются через час.
package main

import (
	"archive/zip"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"tinyimg/internal/compress"
	imgweb "tinyimg/web"
)

const (
	maxFileSize  = 100 << 20 // 100 МБ на файл
	maxFiles     = 20        // как у TinyPNG
	resultTTL    = time.Hour
	compressJobs = 4 // параллельные сжатия (большие файлы жрут память — больше не надо)
	// maxStoreBytes — глобальный бюджет памяти под результаты. TTL не спасает:
	// лимитер per-IP, а карта общая — без потолка N адресов за час набили бы
	// сколько угодно гигабайт. При переполнении вытесняются самые старые.
	maxStoreBytes = 1 << 30 // 1 ГБ
	// formMemory — сколько multipart-парсер держит тела в RAM; остальное —
	// во временный файл на диске. До семафора заданий память соединений не
	// ограничена ничем, поэтому держать по ~100 МБ в RAM нельзя.
	formMemory = 8 << 20
	// maxZipIDs / maxZipBytes — потолки /api/zip: без них один запрос
	// стримит произвольное число копий чужих результатов.
	maxZipIDs   = 40
	maxZipBytes = 512 << 20
)

// Настройки, переопределяемые флагами командной строки. Значения по
// умолчанию — те же, что были до флагов: без них сервис ведёт себя
// ровно как раньше, но на маленьком VPS (2 ядра, 4 ГБ RAM) глобальный
// семафор в 4 параллельных сжатия и 1 ГБ под результаты — это OOM
// в общий котёл с соседними сервисами.
var (
	jobsLimit  = compressJobs
	storeLimit = int64(maxStoreBytes)
)

// result — один сжатый файл, лежащий в памяти до скачивания.
type result struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Before   int64     `json:"before"`
	After    int64     `json:"after"`
	Strategy string    `json:"strategy"`
	Data     []byte    `json:"-"`
	Created  time.Time `json:"-"`
}

type server struct {
	mu         sync.Mutex
	results    map[string]*result
	storeBytes int64 // суммарный размер Data всех результатов
	maxStore   int64 // бюджет storeBytes (поле — чтобы тесты могли уменьшить)
	jobs       chan struct{}
	pageTmpl   []byte
	limiter    *ipLimiter // защита от перегрузки по IP (nil = выключена)
	proxies    []*net.IPNet
	stopSweep  chan struct{}
}

func newServer(limiter *ipLimiter, proxies ...*net.IPNet) *server {
	s := &server{
		results:   map[string]*result{},
		maxStore:  storeLimit,
		jobs:      make(chan struct{}, jobsLimit),
		limiter:   limiter,
		proxies:   proxies,
		stopSweep: make(chan struct{}),
	}
	go s.sweep()
	return s
}

// close — остановить фоновую уборку (тесты; в main живёт вечно).
func (s *server) close() { close(s.stopSweep) }

// sweep — удаляет результаты старше resultTTL.
func (s *server) sweep() {
	tick := time.NewTicker(10 * time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			s.mu.Lock()
			for id, r := range s.results {
				if time.Since(r.Created) > resultTTL {
					s.storeBytes -= int64(len(r.Data))
					delete(s.results, id)
				}
			}
			s.mu.Unlock()
		case <-s.stopSweep:
			return
		}
	}
}

// put — сохранить результат с соблюдением глобального бюджета памяти:
// при переполнении вытесняем самые старые записи (LRU по Created).
func (s *server) put(r *result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.results[r.ID] = r
	s.storeBytes += int64(len(r.Data))
	for s.storeBytes > s.maxStore && len(s.results) > 1 {
		var oldID string
		var oldTime time.Time
		for id, res := range s.results {
			if id == r.ID { // сам себя не вытесняем
				continue
			}
			if oldID == "" || res.Created.Before(oldTime) {
				oldID, oldTime = id, res.Created
			}
		}
		if oldID == "" {
			break
		}
		s.storeBytes -= int64(len(s.results[oldID].Data))
		delete(s.results, oldID)
	}
}

func (s *server) get(id string) *result {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.results[id]
}

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func main() {
	addr := ":8080"
	rate := 60  // запросов на IP в минуту; 0 = без лимита
	burst := 30 // запас корзины: батч из 20 картинок должен проходить целиком
	conc := 4   // одновременных сжатий на IP
	args := os.Args[1:]
	var proxyArgs []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-addr":
			if i+1 < len(args) {
				i++
				addr = args[i]
			} else {
				log.Fatal("флаг -addr требует значение")
			}
		case "-rate":
			v, err := nextInt(args, &i, "-rate")
			if err != nil {
				log.Fatal(err)
			}
			rate = v
		case "-burst":
			v, err := nextInt(args, &i, "-burst")
			if err != nil {
				log.Fatal(err)
			}
			burst = v
		case "-conc":
			v, err := nextInt(args, &i, "-conc")
			if err != nil {
				log.Fatal(err)
			}
			conc = v
		case "-trust-proxy":
			if i+1 < len(args) {
				i++
				proxyArgs = append(proxyArgs, args[i])
			} else {
				log.Fatal("флаг -trust-proxy требует значение (CIDR через запятую)")
			}
		case "-jobs":
			v, err := nextInt(args, &i, "-jobs")
			if err != nil {
				log.Fatal(err)
			}
			if v < 1 {
				log.Fatal("флаг -jobs требует значение >= 1")
			}
			jobsLimit = v
		case "-store-mb":
			v, err := nextInt(args, &i, "-store-mb")
			if err != nil {
				log.Fatal(err)
			}
			if v < 1 {
				log.Fatal("флаг -store-mb требует значение >= 1")
			}
			storeLimit = int64(v) << 20
		default:
			log.Fatalf("неизвестный флаг: %s", args[i])
		}
	}
	proxies, err := parseProxies(proxyArgs)
	if err != nil {
		log.Fatal(err)
	}
	srv := &http.Server{
		Addr:    addr,
		Handler: newMux(newIPLimiter(rate, burst, conc), proxies...),
		// Медленные соединения не должны висеть вечно: без общего
		// ReadTimeout slowloris держит и слот лимитера, и горутину.
		// 5 минут хватает на аплоад 100 МБ с ~333 КБ/с. WriteTimeout не
		// ставим: /api/zip стримит большие архивы.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       5 * time.Minute,
		IdleTimeout:       60 * time.Second,
	}
	log.Printf("tinyimg-web на http://localhost%s (лимит %d запросов/мин на IP, burst %d, %d сжатий одновременно, %d МБ под результаты, webp: %v)",
		addr, rate, burst, conc, storeLimit>>20, compress.WebPSupported())
	if len(proxies) > 0 {
		log.Printf("доверенные прокси (их X-Forwarded-For учитывается): %v", proxyArgs)
	}
	log.Fatal(srv.ListenAndServe())
}

// nextInt — разбор целочисленного значения флага с проверкой диапазона.
func nextInt(args []string, i *int, name string) (int, error) {
	if *i+1 >= len(args) {
		return 0, fmt.Errorf("флаг %s требует значение", name)
	}
	*i++
	v, err := strconv.Atoi(args[*i])
	if err != nil {
		return 0, fmt.Errorf("неверное значение %s: %q (ожидается целое число)", name, args[*i])
	}
	if v < 0 {
		return 0, fmt.Errorf("неверное значение %s: %q (ожидается >= 0)", name, args[*i])
	}
	return v, nil
}

// parseProxies — список доверенных прокси (CIDR или IP через запятую).
// Только их X-Forwarded-For учитывается: иначе заголовок подделывается
// любым клиентом и лимит обходится.
func parseProxies(vals []string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, v := range vals {
		for _, part := range strings.Split(v, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if _, n, err := net.ParseCIDR(part); err == nil {
				out = append(out, n)
				continue
			}
			ip := net.ParseIP(part)
			if ip == nil {
				return nil, fmt.Errorf("неверный адрес прокси: %q (ожидается IP или CIDR)", part)
			}
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
		}
	}
	return out, nil
}

func newMux(limiter *ipLimiter, proxies ...*net.IPNet) http.Handler {
	s := newServer(limiter, proxies...)
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(imgweb.FS()))
	mux.HandleFunc("GET /site.webmanifest", s.handleManifest)
	mux.HandleFunc("POST /api/compress", s.handleCompress)
	mux.HandleFunc("GET /api/file/{id}", s.handleDownload)
	mux.HandleFunc("GET /api/zip", s.handleZip)
	mux.HandleFunc("GET /api/features", s.handleFeatures)
	mux.HandleFunc("GET /mcp", s.handleMCPInfo)
	mux.HandleFunc("POST /mcp", s.handleMCP)
	return mux
}

// handleManifest — отдаёт webmanifest с правильным MIME-типом.
// Go не знает расширения .webmanifest (mime.TypeByExtension вернёт пусто),
// и FileServer отдал бы файл как application/octet-stream: браузер
// (Chrome) тогда отвергнет манифест молча — PWA-иконки не появятся.
func (s *server) handleManifest(w http.ResponseWriter, r *http.Request) {
	data, err := fs.ReadFile(imgweb.FS(), "site.webmanifest")
	if err != nil {
		http.Error(w, "манифест недоступен", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/manifest+json; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Write(data)
}

// handleFeatures — что умеет эта сборка (например, доступен ли webp-кодек).
// Фронтенд прячет обещание WebP, если кодек недоступен.
func (s *server) handleFeatures(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"webp": compress.WebPSupported(),
		"formats": []string{"png", "jpeg", "webp"},
	})
}

type compressResponse struct {
	OK    bool    `json:"ok"`
	Error string  `json:"error,omitempty"`
	File  *result `json:"file,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (s *server) handleCompress(w http.ResponseWriter, r *http.Request) {
	// Защита от перегрузки: токен-бакет на IP + лимит одновременных сжатий.
	ip := ipFromRequest(r, s.proxies)
	if ok, retry := s.limiter.allow(ip); !ok {
		s.limiter.reject(w, retry)
		return
	}
	if !s.limiter.acquire(ip) {
		s.limiter.reject(w, 2*time.Second)
		return
	}
	defer s.limiter.release(ip)

	// Ограничение тела: файл + multipart-накладные. maxMemory маленький:
	// тело сразу уходит во временный файл, а не висит в RAM до семафора.
	r.Body = http.MaxBytesReader(w, r.Body, maxFileSize+1<<20)
	if err := r.ParseMultipartForm(formMemory); err != nil {
		writeJSON(w, http.StatusBadRequest, compressResponse{Error: "файл слишком большой или форма повреждена"})
		return
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, compressResponse{Error: "ожидается файл в поле file"})
		return
	}
	defer f.Close()

	name := filepath.Base(hdr.Filename)
	ext := strings.ToLower(filepath.Ext(name))
	if !compress.IsSupportedExt(ext) {
		writeJSON(w, http.StatusBadRequest, compressResponse{Error: "нужен " + compress.SupportedExts() + ": " + name})
		return
	}
	if ext == ".webp" && !compress.WebPSupported() {
		writeJSON(w, http.StatusBadRequest, compressResponse{Error: "эта сборка сервера без WebP-кодека (нужен cgo + libwebp)"})
		return
	}
	if hdr.Size > maxFileSize {
		writeJSON(w, http.StatusRequestEntityTooLarge, compressResponse{Error: fmt.Sprintf("слишком большой файл (макс. %d МБ): %s", maxFileSize>>20, name)})
		return
	}

	// Сжатие — CPU-тяжёлое: ограничиваем параллельность.
	s.jobs <- struct{}{}
	defer func() { <-s.jobs }()

	res, err := s.compressUpload(f, name)
	if err != nil {
		log.Printf("ошибка сжатия %s: %v", name, err)
		writeJSON(w, http.StatusInternalServerError, compressResponse{Error: "не удалось сжать: " + name})
		return
	}
	writeJSON(w, http.StatusOK, compressResponse{OK: true, File: res})
}

// compressUpload сохраняет загрузку во временный файл, жмёт через
// internal/compress (то же, что CLI) и забирает результат в память.
func (s *server) compressUpload(f multipart.File, name string) (*result, error) {
	dir, err := os.MkdirTemp("", "tinyimg-web-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	src := filepath.Join(dir, "src"+filepath.Ext(name))
	dst := filepath.Join(dir, "out"+filepath.Ext(name))
	out, err := os.Create(src)
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(out, f); err != nil {
		out.Close()
		return nil, err
	}
	if err := out.Close(); err != nil {
		return nil, err
	}

	// squeeze: лосслесс-переупаковка IDAT через zopfli (−6…−7% к размеру) —
	// только для мелких файлов: кодирование стоит ~×25, и на больших файлах
	// пользователь ждал бы десятки секунд ради копейки.
	opts := compress.Options{Dither: -1} // auto: 0.6/9
	if fi, err := os.Stat(src); err == nil && fi.Size() < 500<<10 {
		opts.Squeeze = true
	}
	r := compress.File(src, dst, opts)
	if r.Err != nil {
		return nil, r.Err
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		return nil, err
	}

	res := &result{
		ID: newID(), Name: name,
		Before: r.Before, After: r.After, Strategy: r.Strategy,
		Data: data, Created: time.Now(),
	}
	s.put(res)
	return res, nil
}

func (s *server) handleDownload(w http.ResponseWriter, r *http.Request) {
	res := s.get(r.PathValue("id"))
	if res == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentTypeOf(res.Name))
	// Имя файла — в UTF-8 через filename* (кириллица не ломается).
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="compressed%s"; filename*=UTF-8''%s`,
			filepath.Ext(res.Name), urlEncode(res.Name)))
	w.Write(res.Data)
}

func (s *server) handleZip(w http.ResponseWriter, r *http.Request) {
	// Тот же лимитер, что у сжатия: без него zip — дешёвый усилитель DoS
	// (повторные id → бесконечный поток чужих данных).
	ip := ipFromRequest(r, s.proxies)
	if ok, retry := s.limiter.allow(ip); !ok {
		s.limiter.reject(w, retry)
		return
	}
	ids := strings.Split(r.URL.Query().Get("ids"), ",")
	var results []*result
	seen := map[string]bool{}
	var total int64
	for _, id := range ids {
		if len(results) >= maxZipIDs {
			break
		}
		id = strings.TrimSpace(id)
		if seen[id] {
			continue // дедуп: повторный id не умножает поток
		}
		res := s.get(id)
		if res == nil {
			continue
		}
		seen[id] = true
		total += int64(len(res.Data))
		if total > maxZipBytes {
			writeJSON(w, http.StatusRequestEntityTooLarge,
				compressResponse{Error: "архив слишком большой (лимит 512 МБ)"})
			return
		}
		results = append(results, res)
	}
	if len(results) == 0 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="tinyimg.zip"`)
	zw := zip.NewWriter(w)
	defer zw.Close()
	// Store: картинки уже сжаты, Deflate над ними — чистый расход CPU
	// на нулевой выигрыш.
	used := map[string]bool{}
	for _, res := range results {
		name := res.Name
		for i := 2; used[name]; i++ { // дубликаты имён: photo.png, 2-photo.png…
			name = fmt.Sprintf("%d-%s", i, res.Name)
		}
		used[name] = true
		fw, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
		if err != nil {
			return
		}
		fw.Write(res.Data)
	}
}

func contentTypeOf(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	default:
		return "application/octet-stream"
	}
}

// urlEncode — percent-кодирование для filename* (RFC 5987).
func urlEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
