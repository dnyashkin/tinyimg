// tinyimg — компрессор PNG/JPEG в зоне качества TinyPNG, локально.
//
// Сжимает пакетно или по одному файлу, результат пишет в каталог.
// Без зависимостей (чистый Go), без загрузки картинок в чужие сервисы.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"tinyimg/internal/compress"
)

// version — перекрывается при сборке:
//
//	go build -ldflags "-X main.version=1.0.0" ./cmd/tinyimg
var version = "0.1.0"

const usage = `tinyimg — компрессор PNG/JPEG в зоне качества TinyPNG (локально)

использование:
  tinyimg <файл | папка> [флаги]

флаги:
  -o, --output DIR     каталог результата (по умолчанию: output)
  -d, --dither MODE    дизеринг палитры PNG: auto | none | mild | full | 0..1
                       (auto = 0.6 — шум против ступеней градиентов)
      --clamp N        амплитуда дизеринга в уровнях 1..255 (auto = 9;
                       выше — сильнее размывает ступени градиентов, но заметнее шум)
      --squeeze        лосслесс-переупаковка PNG через zopfli-энкодер: −6…−7%
                       к размеру, но ~×25 к времени сжатия
  -j, --jobs N         параллельные файлы (по умолчанию: min(ядер, 6))
  -q, --quiet          только ошибки и итоговая строка
      --json           вывести результаты в stdout как JSON
  -v, --verbose        время по стадиям в stderr
      --version        версия
  -h, --help           эта справка

примеры:
  tinyimg photo.png                     один файл -> output/photo.png
  tinyimg pics/ -o out                  папка рекурсивно -> out/
  tinyimg pics/ --dither none           без дизеринга (жёсткие палитры)
  tinyimg pics/ --json | jq .total      для скриптов

стратегия: PNG — lossless-переупак или палитра ≤256 (median-cut + Ллойд,
дозированный Floyd–Steinberg); JPEG — пережатие в зоне q50–60, файлы <100 КБ
и уже оптимальные не трогаем; метаданные вырезаются.
Замеры и исследование: research/BENCH.md, research/report.md.

коды возврата: 0 — успех, 1 — ошибки обработки, 2 — неверные аргументы`

type jsonResult struct {
	Name          string  `json:"name"`
	Before        int64   `json:"before"`
	After         int64   `json:"after"`
	SavedPercent  float64 `json:"saved_percent"`
	DurationMs    int64   `json:"duration_ms"`
	Strategy      string  `json:"strategy"`
	Error         string  `json:"error,omitempty"`
}

type jsonReport struct {
	Results []jsonResult `json:"results"`
	Total   struct {
		Before int64 `json:"before"`
		After  int64 `json:"after"`
		WallMs int64 `json:"wall_ms"`
	} `json:"total"`
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	opts := compress.Options{Dither: -1} // auto
	outDir := "output"
	quiet, jsonOut := false, false
	var positional []string

	for i := 0; i < len(args); i++ {
		a := args[i]
		needValue := func() (string, bool) {
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "ошибка: у флага %s не указано значение\n", a)
				return "", false
			}
			i++
			return args[i], true
		}
		switch {
		case a == "-h" || a == "--help":
			fmt.Println(usage)
			return 0
		case a == "--version":
			fmt.Printf("tinyimg %s (%s, %s/%s)\n", version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
			return 0
		case a == "-q" || a == "--quiet":
			quiet = true
		case a == "--json":
			jsonOut = true
		case a == "-v" || a == "--verbose":
			opts.Verbose = true
		case a == "--squeeze":
			opts.Squeeze = true
		case a == "--clamp":
			v, ok := needValue()
			if !ok {
				return 2
			}
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 255 {
				fmt.Fprintf(os.Stderr, "ошибка: --clamp ждёт 1..255 уровней, получено %q\n", v)
				return 2
			}
			opts.Clamp = n
		case a == "-o" || a == "--output":
			v, ok := needValue()
			if !ok {
				return 2
			}
			outDir = v
		case a == "-j" || a == "--jobs":
			v, ok := needValue()
			if !ok {
				return 2
			}
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				fmt.Fprintf(os.Stderr, "ошибка: --jobs ждёт положительное число, получено %q\n", v)
				return 2
			}
			opts.Jobs = n
		case a == "-d" || a == "--dither":
			v, ok := needValue()
			if !ok {
				return 2
			}
			switch v {
			case "none":
				opts.Dither = 0
			case "auto":
				opts.Dither = -1
			case "mild":
				opts.Dither = 0.4
				if opts.Clamp == 0 {
					opts.Clamp = 6
				}
			case "full":
				opts.Dither = 1.0
				if opts.Clamp == 0 {
					opts.Clamp = 255
				}
			default:
				f, err := strconv.ParseFloat(v, 64)
				if err != nil || f < 0 || f > 1 {
					fmt.Fprintf(os.Stderr, "ошибка: --dither ждёт auto | none | mild | full | 0..1, получено %q\n", v)
					return 2
				}
				opts.Dither = f
			}
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(os.Stderr, "ошибка: неизвестный флаг %q (см. --help)\n", a)
			return 2
		default:
			positional = append(positional, a)
		}
	}

	if len(positional) != 1 {
		fmt.Fprintln(os.Stderr, "ошибка: нужен ровно один вход — файл или папка (см. --help)")
		return 2
	}
	input := positional[0]

	st, err := os.Stat(input)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ошибка: %v\n", err)
		return 1
	}

	var results []compress.Result
	wallStart := time.Now()
	if st.IsDir() {
		results, err = compress.Dir(input, outDir, opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ошибка: %v\n", err)
			return 1
		}
	} else {
		results = []compress.Result{compress.File(input, filepath.Join(outDir, filepath.Base(input)), opts)}
	}
	wall := time.Since(wallStart)

	if len(results) == 0 {
		fmt.Fprintln(os.Stderr, "PNG/JPEG файлы не найдены")
		return 1
	}

	var totalBefore, totalAfter int64
	failed := 0
	for _, r := range results {
		if r.Err != nil {
			failed++
			fmt.Fprintf(os.Stderr, "✗ %s: %v\n", r.Name, r.Err)
			continue
		}
		totalBefore += r.Before
		totalAfter += r.After
	}

	switch {
	case jsonOut:
		report := jsonReport{Results: make([]jsonResult, 0, len(results))}
		for _, r := range results {
			jr := jsonResult{
				Name: r.Name, Before: r.Before, After: r.After,
				SavedPercent: round1(r.SavedPercent()),
				DurationMs:   r.Duration.Milliseconds(), Strategy: r.Strategy,
			}
			if r.Err != nil {
				jr.Error = r.Err.Error()
			}
			report.Results = append(report.Results, jr)
		}
		report.Total.Before, report.Total.After = totalBefore, totalAfter
		report.Total.WallMs = wall.Milliseconds()
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			fmt.Fprintf(os.Stderr, "ошибка: %v\n", err)
			return 1
		}
	case !quiet:
		for _, r := range results {
			if r.Err != nil {
				continue
			}
			mbs := float64(r.Before) / 1024 / 1024 / r.Duration.Seconds()
			fmt.Printf("✓ %-40s %8.1f КБ → %8.1f КБ  (−%4.1f%%)  %6.2fs %5.1f МБ/с  %s\n",
				r.Name, kb(r.Before), kb(r.After), r.SavedPercent(),
				r.Duration.Seconds(), mbs, r.Strategy)
		}
	}
	if totalBefore > 0 {
		saved := 100 - float64(totalAfter)*100/float64(totalBefore)
		jobs := opts.Jobs
		if jobs <= 0 {
			jobs = runtime.GOMAXPROCS(0)
			if jobs > 6 {
				jobs = 6
			}
		}
		fmt.Printf("\nитого: %.2f МБ → %.2f МБ (−%.1f%%) за %.1f с wall (%.1f МБ/с), %d потоков\n",
			mb(totalBefore), mb(totalAfter), saved,
			wall.Seconds(), float64(totalBefore)/1024/1024/wall.Seconds(), jobs)
	}
	if failed > 0 {
		return 1
	}
	return 0
}

func kb(n int64) float64 { return float64(n) / 1024 }
func mb(n int64) float64 { return float64(n) / 1024 / 1024 }

func round1(v float64) float64 { return float64(int(v*10+0.5)) / 10 }
