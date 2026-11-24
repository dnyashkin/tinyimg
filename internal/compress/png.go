package compress

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"sort"
	"time"
)

// compressPNG — пайплайн PNG: сначала дешёвая стратегия по числу цветов,
// потом палитровая квантизация с дизерингом.
func compressPNG(src, dst string, opts Options, stage *stageLogger) (before, after int64, strategy string, err error) {
	raw, err := os.ReadFile(src)
	if err != nil {
		return 0, 0, "", err
	}
	before = int64(len(raw))
	// Сначала только заголовок: отсекаем «бомбы распаковки» (огромные
	// размеры при крошечном файле) до выделения памяти под пиксели.
	cfg, err := png.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return 0, 0, "", fmt.Errorf("декод: %w", err)
	}
	if err := checkDims(cfg.Width, cfg.Height, src); err != nil {
		return 0, 0, "", err
	}
	t0 := time.Now()
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return 0, 0, "", fmt.Errorf("декод: %w", err)
	}
	stage.log("decode", t0)

	// 16-битные источники: конверсия в 8-битную палитру теряла бы глубину
	// цвета под вывеской «без потерь». Для них — только лосслесс-переупак.
	switch img.(type) {
	case *image.NRGBA64, *image.RGBA64, *image.Gray16, *image.Alpha16:
		lossless, err := encodePNG(img, png.DefaultCompression)
		if err != nil {
			return 0, 0, "", err
		}
		if opts.Squeeze {
			lossless = repackPNG(lossless)
		}
		if len(lossless) >= len(raw) {
			// Уже оптимален (часто после TinyPNG): раздувать нельзя —
			// тот же гард, что у JPEG/WebP, иначе повторный прогон растит файл.
			if err := writeOut(dst, raw); err != nil {
				return 0, 0, "", err
			}
			return before, before, "уже оптимален (не трогаем)", nil
		}
		if err := writeOut(dst, lossless); err != nil {
			return 0, 0, "", err
		}
		return before, int64(len(lossless)), "lossless (16-бит, без палитры)", nil
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()

	// Сколько уникальных цветов — решает стратегию. Гистограмма с весами
	// (а не set): тот же проход кормит и итерации Ллойда ниже — одна map
	// на весь файл вместо двух полных построений.
	t2 := time.Now()
	pixels := make([][4]uint8, w*h)
	hist := make(map[[4]uint8]int, 1<<16)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := color.NRGBAModel.Convert(img.At(b.Min.X+x, b.Min.Y+y)).(color.NRGBA)
			p := [4]uint8{c.R, c.G, c.B, c.A}
			pixels[y*w+x] = p
			hist[p]++
		}
	}
	stage.log("pixels+hist", t2)
	unique := len(hist)

	if unique <= 256 {
		// Палитра без потерь: идеальное качество + палитровое сжатие.
		// Сравниваем с lossless-переупаком (Default: проба должна быть дешёвой —
		// BestCompression на фотошных данных ест минуты).
		t1 := time.Now()
		lossless, err := encodePNG(img, png.DefaultCompression)
		if err != nil {
			return 0, 0, "", err
		}
		stage.log("lossless-encode", t1)
		palImg := buildPaletted(w, h, pixels, paletteFrom(hist), 0, 0, 0)
		palBytes, err := encodeBest(palImg)
		if err == nil && len(palBytes) < len(lossless) {
			lossless = palBytes
			strategy = "lossless (палитра без потерь)"
		} else {
			strategy = "lossless (переупак)"
		}
		if opts.Squeeze {
			lossless = repackPNG(lossless)
		}
		if len(lossless) >= len(raw) {
			// Гард «не раздувать»: уже оптимальный файл остаётся как есть.
			if err := writeOut(dst, raw); err != nil {
				return 0, 0, "", err
			}
			return before, before, "уже оптимален (не трогаем)", nil
		}
		if err := writeOut(dst, lossless); err != nil {
			return 0, 0, "", err
		}
		return before, int64(len(lossless)), strategy, nil
	}

	// Палитровая квантизация ≤256 (median-cut + Ллойд).
	// Дизеринг auto: сила 0.5, clamp ±8 — подобрано по корпусу вместе со
	// взвешенным сплитом median-cut (см. boxPopExp в quantize.go): лесенка
	// на градиентах размывается, шум остаётся ниже порога заметности, и
	// размер фото-PNG держится в гейте против TinyPNG.
	t3 := time.Now()
	pal := medianCut(pixels, 256)
	pal = lloydRefine(hist, pal, 12)
	stage.log("quantize", t3)
	strength := opts.Dither
	if strength < 0 {
		strength = 0.5 // auto: дозированный шум — иначе «лесенка»
		// на крупных градиентах (сравнение с TinyPNG по bias-метрике,
		// см. README «Как сжимает»).
	}
	clamp := opts.Clamp
	if clamp <= 0 {
		clamp = 8
	}
	t4 := time.Now()
	palImg := buildPaletted(w, h, pixels, pal, strength, clamp, opts.EntropyTie)
	stage.log("map-pixels", t4)
	t5 := time.Now()
	palBytes, err := encodeBest(palImg)
	stage.log("palette-encode", t5)
	if err != nil {
		return 0, 0, "", err
	}

	// Правило Q9: lossless-переупак остаётся, если он в пределах +20% от
	// палитрового результата (качество 100% почти даром). Пробуем только
	// при ≤4096 уникальных цветов: на фотошных данных пережатие без потерь
	// заведомо проигрывает палитре в разы, а проба стоит десятки секунд.
	// Переупак, раздувающий вход, нам не нужен независимо от палитры.
	if unique <= 4096 {
		t1 := time.Now()
		lossless, err := encodePNG(img, png.DefaultCompression)
		stage.log("lossless-encode", t1)
		if err == nil && len(lossless) < len(raw) && float64(len(lossless)) <= 1.2*float64(len(palBytes)) {
			if opts.Squeeze {
				lossless = repackPNG(lossless)
			}
			if len(lossless) < len(raw) { // squeeze мог «не помочь» и вернуть вход
				if err := writeOut(dst, lossless); err != nil {
					return 0, 0, "", err
				}
				return before, int64(len(lossless)), "lossless (переупак)", nil
			}
		}
	}
	if opts.Squeeze {
		palBytes = repackPNG(palBytes)
	}
	if len(palBytes) >= len(raw) {
		// Гард «не раздувать» (как у JPEG/WebP): исходник лучше любого
		// кандидата — отдаём его байт-в-байт, повторный прогон не растит файл.
		if err := writeOut(dst, raw); err != nil {
			return 0, 0, "", err
		}
		return before, before, "уже оптимален (не трогаем)", nil
	}
	if err := writeOut(dst, palBytes); err != nil {
		return 0, 0, "", err
	}
	return before, int64(len(palBytes)), "палитра 256 (median-cut+Ллойд, " + ditherName(strength) + ")", nil
}

func ditherName(strength float64) string {
	switch strength {
	case 0:
		return "без дизеринга"
	case 0.4:
		return "FS mild 0.4"
	case 0.5:
		return "FS auto 0.5"
	case 1.0:
		return "FS full"
	}
	return fmt.Sprintf("FS %.2f", strength)
}

func encodeBest(img image.Image) ([]byte, error) {
	return encodePNG(img, png.BestCompression)
}

func encodePNG(img image.Image, level png.CompressionLevel) ([]byte, error) {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: level}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func paletteFrom(hist map[[4]uint8]int) [][4]uint8 {
	pal := make([][4]uint8, 0, len(hist))
	for p := range hist {
		pal = append(pal, p)
	}
	// Порядок записей фиксируем: обход map случаен, и без сортировки
	// PLTE-чанк (а значит и байты выхода) гулял от запуска к запуску.
	sort.Slice(pal, func(i, j int) bool {
		a, b := pal[i], pal[j]
		for k := 0; k < 4; k++ {
			if a[k] != b[k] {
				return a[k] < b[k]
			}
		}
		return false
	})
	return pal
}

// buildPaletted — палитровое изображение; strength > 0 включает
// Floyd–Steinberg (змейка, диффузия только по RGB с весом альфы —
// шумить прозрачность нельзя, это рвёт края наклеек).
// tie > 0 — энтропийный tie-break: при почти равных цветах палитры берём
// индекс левого/верхнего соседа (усиливает LZ77-матчи в deflate).
func buildPaletted(w, h int, pixels [][4]uint8, pal [][4]uint8, strength float64, clamp int, tie float64) *image.Paletted {
	palCol := make(color.Palette, len(pal))
	for i, p := range pal {
		// color.NRGBA — прямые (не premultiplied) значения, как в pixels.
		palCol[i] = color.NRGBA{p[0], p[1], p[2], p[3]}
	}
	img := image.NewPaletted(image.Rect(0, 0, w, h), palCol)
	pi := newPalIndex(pal)

	if strength <= 0 {
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				img.SetColorIndex(x, y, pi.nearest(pixels[y*w+x]))
			}
		}
		return img
	}

	// Диффузия ошибки: две строки RGB-ошибок, змейка (serpentine).
	maxErr := float64(clamp) // амплитуда ограничена: шум дозирован
	cur := make([][3]float64, w+2)
	next := make([][3]float64, w+2)
	for y := 0; y < h; y++ {
		dir, start := 1, 0
		if y%2 == 1 {
			dir, start = -1, w-1
		}
		for i := 0; i < w; i++ {
			x := start + i*dir
			leftIdx := -1
			if i > 0 {
				leftIdx = int(img.Pix[y*img.Stride+start+(i-1)*dir])
			}
			p := pixels[y*w+x]
			var cur4 [4]uint8
			for c := 0; c < 3; c++ {
				v := float64(p[c]) + cur[x+1][c]
				if v < 0 {
					v = 0
				} else if v > 255 {
					v = 255
				}
				cur4[c] = uint8(v + 0.5)
			}
			cur4[3] = p[3] // альфа — точно, без диффузии
			idx := pi.nearest(cur4)

			// Энтропийный tie-break: если индекс соседа (левого — пробег,
			// верхнего — повтор строки) даёт почти ту же ошибку, берём его.
			// Стандартный nearest не знает про соседей; тут дешёвое решение
			// «на лету», качество почти не меняется (ошибка уходит в диффузию).
			if tie > 0 && y > 0 && leftIdx >= 0 {
				bestD := sqDist(cur4, pal[idx])
				limit := float64(bestD) * (1 + tie)
				upIdx := int(img.Pix[(y-1)*img.Stride+x])
				// Переворачиваем, только когда оба соседа согласны:
				// так растут 2D-регионы (надёжные LZ77-матчи), а возмущение
				// диффузии минимально. Одиночный tie слева/сверху на фото
				// возмущает поток ошибки и даёт +2% размера (проверено).
				if leftIdx == upIdx && uint8(leftIdx) != idx && float64(sqDist(cur4, pal[leftIdx])) <= limit {
					idx = uint8(leftIdx)
				}
			}
			img.SetColorIndex(x, y, idx)
			leftIdx = int(idx)

			// Ошибка уходит в будущее: вперёд по строке и в строку ниже.
			// Вес альфы: в прозрачности ошибки «не существует».
			var err [3]float64
			for c := 0; c < 3; c++ {
				err[c] = float64(cur4[c]) - float64(pal[idx][c])
			}
			scale := strength * float64(p[3]) / 255
			if scale == 0 {
				continue
			}
			for c := 0; c < 3; c++ {
				e := err[c] * scale
				// Ограничение амплитуды: шум размывает бандинг,
				// но не «раскачивает» соседние пиксели.
				if e > maxErr {
					e = maxErr
				} else if e < -maxErr {
					e = -maxErr
				}
				err[c] = e
			}
			add := func(xx int, row [][3]float64, f float64) {
				if xx < 0 || xx >= w {
					return
				}
				for c := 0; c < 3; c++ {
					row[xx+1][c] += err[c] * f
				}
			}
			add(x+dir, cur, 7.0/16)
			add(x-dir, next, 3.0/16)
			add(x, next, 5.0/16)
			add(x+dir, next, 1.0/16)
		}
		cur, next = next, cur
		for j := range next {
			next[j] = [3]float64{}
		}
	}
	return img
}
