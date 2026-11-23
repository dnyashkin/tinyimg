// Package compress — сжатие PNG/JPEG/WebP в зоне качества TinyPNG, локально.
//
// PNG: lossless-переупак либо палитровая квантизация ≤256 цветов
// (median-cut + доводка Ллойда) с дозированным дизерингом Флойда–Стейнберга.
// JPEG: пережатие в зону качества TinyPNG (q≈50–60 stdlib), метаданные
// вырезаются, мелкие и уже оптимальные файлы не трогаются.
// WebP: через libwebp (cgo; в сборке без cgo — заглушка с ошибкой),
// lossless-вход сравнивается с lossless-переупаком и lossy-свипом при
// SSIM ≥ 0.98, берётся меньший кандидат.
package compress

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// MaxPixels — потолок размеров изображения в пикселях (ширина × высота).
// Лимит размера ФАЙЛА не защищает от «бомбы распаковки»: сплошная заливка
// 50000×50000 жмётся в PNG на десятки килобайт, а декод и буферы пикселей
// развернут её в десятки гигабайт. 100 Мпикс ≈ 400 МБ RGBA — верхняя
// граница разумного для сервиса и CLI.
const MaxPixels = 100_000_000

// checkDims — проверка размеров ДО полного декода (через DecodeConfig /
// WebPGetFeatures читаются только заголовки, память не выделяется).
func checkDims(w, h int, src string) error {
	if w <= 0 || h <= 0 || int64(w)*int64(h) > MaxPixels {
		return fmt.Errorf("слишком большие размеры: %dx%d пикселей (лимит %d Мпикс): %s",
			w, h, MaxPixels/1_000_000, src)
	}
	return nil
}

// Options — настройки сжатия.
type Options struct {
	// Dither — сила дизеринга палитры PNG: <0 = auto (0.4), 0 = без дизеринга.
	Dither float64
	// Clamp — ограничение амплитуды ошибки дизеринга (уровней): 0 = auto (6).
	// Выше → активнее размывает ступени градиентов, но заметнее шум.
	Clamp int
	// EntropyTie — допуск tie-break к индексу соседа (левого/верхнего):
	// если соседний цвет хуже лучшего не более чем на tie долей, берём его
	// индекс — так падает энтропия потока индексов и deflate сжимает лучше
	// при неизменном качестве (ошибка почти та же, диффузия её компенсирует).
	// 0 = выключено.
	EntropyTie float64
	// Squeeze — лосслесс-переупаковка IDAT через zopfli-энкодер: −6…−7%
	// к размеру PNG, но ~×25 к времени кодирования. Качество идентично
	// (пиксели не меняются, пережимается только deflate-поток).
	Squeeze bool
	// Jobs — сколько файлов обрабатывать параллельно; <=0 = auto (min(ядер, 6)).
	Jobs int
	// Verbose — выводить время по стадиям в stderr (или STAGE_TIMING=1).
	Verbose bool
}

// Result — итог по одному файлу.
type Result struct {
	Name     string        // относительный путь
	Before   int64         // размер источника, байт
	After    int64         // размер результата, байт
	Strategy string        // применённая стратегия
	Duration time.Duration // время обработки
	Err      error         // nil при успехе
}

// SavedPercent — экономия в процентах.
func (r Result) SavedPercent() float64 {
	if r.Before == 0 {
		return 0
	}
	return 100 - float64(r.After)*100/float64(r.Before)
}

// File — сжать один PNG/JPEG/WebP из src в dst.
func File(src, dst string, opts Options) Result {
	start := time.Now()
	before, after, strategy, err := compressFile(src, dst, opts)
	return Result{
		Name: filepath.Base(src), Before: before, After: after,
		Strategy: strategy, Duration: time.Since(start), Err: err,
	}
}

// Dir — сжать все PNG/JPEG/WebP из srcDir в dstDir (рекурсивно, структура
// сохраняется). Порядок результатов совпадает с порядком файлов.
func Dir(srcDir, dstDir string, opts Options) ([]Result, error) {
	files, err := collectImages(srcDir)
	if err != nil {
		return nil, err
	}
	jobs := opts.Jobs
	if jobs <= 0 {
		jobs = runtime.GOMAXPROCS(0)
		// Большие PNG жрут по ~1 ГБ памяти: на 16 ГБ машине держим ≤6.
		if jobs > 6 {
			jobs = 6
		}
	}
	results := make([]Result, len(files))
	// Worker-pool: фиксированное число рабочих вместо горутины на файл —
	// на каталоге в десятки тысяч файлов не плодим столько же спящих горутин.
	work := make(chan int)
	var wg sync.WaitGroup
	for range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				f := files[i]
				r := File(f.path, filepath.Join(dstDir, f.rel), opts)
				r.Name = f.rel
				results[i] = r
			}
		}()
	}
	for i := range files {
		work <- i
	}
	close(work)
	wg.Wait()
	return results, nil
}

type inputFile struct{ path, rel string }

// supportedExt — единственный источник правды по поддерживаемым форматам:
// используется и фильтром сбора, и диспетчером, и текстами ошибок.
var supportedExt = map[string]bool{
	".png":  true,
	".jpg":  true,
	".jpeg": true,
	".webp": true,
}

// SupportedExts — список поддерживаемых расширений (через запятую) для
// сообщений об ошибках и документации.
func SupportedExts() string {
	return "PNG, JPEG и WebP"
}

// IsSupportedExt — поддерживается ли такое расширение.
func IsSupportedExt(ext string) bool {
	return supportedExt[strings.ToLower(ext)]
}

func collectImages(input string) ([]inputFile, error) {
	isImage := func(p string) bool {
		return IsSupportedExt(filepath.Ext(p))
	}
	st, err := os.Stat(input)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		if !isImage(input) {
			return nil, fmt.Errorf("поддерживаются %s: %s", SupportedExts(), input)
		}
		return []inputFile{{input, filepath.Base(input)}}, nil
	}
	var out []inputFile
	err = filepath.Walk(input, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		if !isImage(p) {
			return nil
		}
		rel, _ := filepath.Rel(input, p)
		out = append(out, inputFile{p, rel})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].rel < out[j].rel })
	return out, err
}

func compressFile(src, dst string, opts Options) (before, after int64, strategy string, err error) {
	stage := newStageLogger(opts)
	ext := strings.ToLower(filepath.Ext(src))
	if !IsSupportedExt(ext) {
		return 0, 0, "", fmt.Errorf("поддерживаются %s: %s", SupportedExts(), src)
	}
	switch ext {
	case ".jpg", ".jpeg":
		return compressJPEG(src, dst)
	case ".webp":
		return compressWebP(src, dst, opts, stage)
	default:
		return compressPNG(src, dst, opts, stage)
	}
}

// stageLogger — разложение времени по стадиям при Verbose (или STAGE_TIMING=1).
type stageLogger struct{ on bool }

func newStageLogger(opts Options) *stageLogger {
	return &stageLogger{on: opts.Verbose || os.Getenv("STAGE_TIMING") != ""}
}

func (s *stageLogger) log(name string, start time.Time) {
	if s.on {
		fmt.Fprintf(os.Stderr, "  [%s %.2fs]\n", name, time.Since(start).Seconds())
	}
}

func writeOut(dst string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}
