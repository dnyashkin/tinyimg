// bias — измеряет «лесенку» (banding) на градиентах.
//
// Зачем отдельный инструмент: SSIM по структуре НЕ видит ступеней
// (они тонут в среднем по окну), а vision-рейтинги между вызовами
// плавают и тем более зависят от масштаба кропа. Banding — это
// СТРУКТУРНОЕ СМЕЩЕНИЕ (константная ошибка внутри окна), поэтому
// меряем его как |среднее ошибки| по окнам, а шум дизеринга — как
// RMS в тех же окнах.
//
//	среднее по окну ошибки ≈ 0  →  шум дизеринга (нулевое среднее)
//	|среднее по окну ошибки| > 0  →  ступень (смещение)
//
// Меньше bias — глаже градиент. Сравниваем наш результат с эталоном
// TinyPNG на ОДНИХ И ТЕХ Ж окнах (окна выровнены по координатам).
//
// Использование:
//
//	go run ./research/bias <orginal/> <tiny_png/> <our_output/> [размер окна]
package main

import (
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// luma — яркость по Rec.709 в float64 0..1 (как SSIM в харнессе).
func luma(img image.Image, x, y int) float64 {
	r, g, b, _ := img.At(x, y).RGBA()
	// RGBA() возвращает премальтиплицированные значения; для непрозрачных
	// пикселей это просто 16-битные каналы.
	return (0.2126*float64(r) + 0.7152*float64(g) + 0.0722*float64(b)) / 65535.0
}

// metrics — bias и RMS по окнам.
type metrics struct {
	Bias    float64 // среднее |среднее ошибки| по окнам
	RMS     float64 // средний RMS по окнам
	P95     float64 // 95-й перцентиль |среднего ошибки| по окнам
	Windows int
}

// compare — ошибка (ours − orig) по окнам window×window.
func compare(orig, other image.Image, window int) metrics {
	b := orig.Bounds()
	if b.Dx() < window || b.Dy() < window {
		return metrics{}
	}
	var m metrics
	means := make([]float64, 0, 1024)
	for oy := b.Min.Y; oy+window <= b.Max.Y; oy += window {
		for ox := b.Min.X; ox+window <= b.Max.X; ox += window {
			var sum, sumSq float64
			n := 0
			for y := oy; y < oy+window; y++ {
				for x := ox; x < ox+window; x++ {
					e := luma(other, x, y) - luma(orig, x, y)
					sum += e
					sumSq += e * e
					n++
				}
			}
			if n == 0 {
				continue
			}
			mean := sum / float64(n)
			means = append(means, math.Abs(mean))
			m.RMS += math.Sqrt(sumSq / float64(n))
			m.Windows++
		}
	}
	if m.Windows == 0 {
		return metrics{}
	}
	m.Bias = 0
	for _, v := range means {
		m.Bias += v
	}
	m.Bias /= float64(len(means))
	m.RMS /= float64(m.Windows)
	sort.Float64s(means)
	idx := int(float64(len(means)-1) * 0.95)
	m.P95 = means[idx]
	return m
}

func decode(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	return img, err
}

func main() {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "использование: bias <orginal/> <tiny_png/> <our_output/> [окно]")
		os.Exit(2)
	}
	window := 32
	if len(os.Args) > 4 {
		fmt.Sscanf(os.Args[4], "%d", &window)
	}

	origDir, tinyDir, ourDir := os.Args[1], os.Args[2], os.Args[3]

	entries, err := os.ReadDir(origDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		os.Exit(1)
	}

	type row struct {
		name       string
		tiny, ours metrics
		delta      float64 // наш bias − эталонный (меньше = лучше)
	}
	var rows []row

	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		origPath := filepath.Join(origDir, e.Name())
		tinyPath := filepath.Join(tinyDir, e.Name())
		ourPath := filepath.Join(ourDir, e.Name())

		origImg, err := decode(origPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "пропуск %s: оригинал не читается (%v)\n", e.Name(), err)
			continue
		}
		tinyImg, err := decode(tinyPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "пропуск %s: нет эталона TinyPNG\n", e.Name())
			continue
		}
		ourImg, err := decode(ourPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "пропуск %s: нет нашего результата\n", e.Name())
			continue
		}

		// окна должны совпадать: считаем по минимальному размеру
		ow, oh := origImg.Bounds().Dx(), origImg.Bounds().Dy()
		tw, th := tinyImg.Bounds().Dx(), tinyImg.Bounds().Dy()
		uw, uh := ourImg.Bounds().Dx(), ourImg.Bounds().Dy()
		w := ow
		if tw < w {
			w = tw
		}
		if uw < w {
			w = uw
		}
		h := oh
		if th < h {
			h = th
		}
		if uh < h {
			h = uh
		}

		crop := func(img image.Image, x0, y0, ww, hh int) image.Image {
			out := image.NewRGBA(image.Rect(0, 0, ww, hh))
			for y := 0; y < hh; y++ {
				for x := 0; x < ww; x++ {
					out.Set(x, y, img.At(x0+x, y0+y))
				}
			}
			return out
		}
		oc := crop(origImg, 0, 0, w, h)
		tc := crop(tinyImg, 0, 0, w, h)
		uc := crop(ourImg, 0, 0, w, h)

		tm := compare(oc, tc, window)
		um := compare(oc, uc, window)
		rows = append(rows, row{name: e.Name(), tiny: tm, ours: um, delta: um.Bias - tm.Bias})
	}

	if len(rows) == 0 {
		fmt.Println("нечего сравнивать: нет файлов с эталоном TinyPNG")
		return
	}

	fmt.Printf("\nBias-метрика (окно %d×%d): среднее |среднее ошибки| по окнам\n", window, window)
	fmt.Println("Меньше bias = глаже градиент. Отрицательная Δ = мы глаже TinyPNG.")
	fmt.Printf("%-52s %10s %10s %10s %10s\n", "файл", "Tiny bias", "наш bias", "Δ", "наш RMS")
	for _, r := range rows {
		mark := " "
		if r.delta < 0 {
			mark = "✓"
		}
		fmt.Printf("%-52s %10.5f %10.5f %9.5f%s %9.5f\n",
			truncate(r.name, 52), r.tiny.Bias, r.ours.Bias, r.delta, mark, r.ours.RMS)
	}

	var sumTiny, sumOurs float64
	worse := 0
	for _, r := range rows {
		sumTiny += r.tiny.Bias
		sumOurs += r.ours.Bias
		if r.delta > 0 {
			worse++
		}
	}
	fmt.Printf("\nСредний bias: TinyPNG %.5f, наш %.5f — %s\n",
		sumTiny/float64(len(rows)), sumOurs/float64(len(rows)),
		map[bool]string{true: "мы глаже", false: "TinyPNG глаже"}[sumOurs <= sumTiny])
	fmt.Printf("Файлов, где мы хуже TinyPNG по bias: %d/%d\n", worse, len(rows))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}
