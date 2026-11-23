package main

// Эксперимент: энтропийный tie-break дизеринга (Options.EntropyTie).
// Свип tie по корпусу PNG: размер, SSIM и bias-метрика «лесенки»
// (оконное |mean(err)| — структурная ошибка; шум дизеринга даёт ~0).
// Качество не должно упасть при падении размера.

import (
	"fmt"
	"image"
	_ "image/png"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	compress "tinyimg/internal/compress"
)

func luma(img image.Image, w, h int) []float64 {
	out := make([]float64, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			out[y*w+x] = 0.299*float64(r>>8) + 0.587*float64(g>>8) + 0.114*float64(b>>8)
		}
	}
	return out
}

func ssim(a, b []float64, w, h int) float64 {
	c1, c2 := 6.5025, 58.5225
	var total float64
	var n int
	for by := 0; by+8 <= h; by += 8 {
		for bx := 0; bx+8 <= w; bx += 8 {
			var ma, mb, va, vb, cov float64
			for y := 0; y < 8; y++ {
				for x := 0; x < 8; x++ {
					pa := a[(by+y)*w+bx+x]
					pb := b[(by+y)*w+bx+x]
					ma += pa
					mb += pb
					va += pa * pa
					vb += pb * pb
					cov += pa * pb
				}
			}
			ma /= 64
			mb /= 64
			va = va/64 - ma*ma
			vb = vb/64 - mb*mb
			cov = cov/64 - ma*mb
			s := ((2*ma*mb + c1) * (2*cov + c2)) / ((ma*ma + mb*mb + c1) * (va + vb + c2))
			total += s
			n++
		}
	}
	return total / float64(n)
}

// bias — среднее |mean(err)| по окнам 8×8 (структурная ошибка = лесенка;
// шум дизеринга даёт нулевое среднее), err по RGB в уровнях 0..255.
func bias(orig, out image.Image, w, h int) (float64, float64) {
	errs := make([]float64, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r1, g1, b1, _ := orig.At(x, y).RGBA()
			r2, g2, b2, _ := out.At(x, y).RGBA()
			e := (float64(r2>>8) - float64(r1>>8) +
				float64(g2>>8) - float64(g1>>8) +
				float64(b2>>8) - float64(b1>>8)) / 3
			errs[y*w+x] = e
		}
	}
	var wins []float64
	for by := 0; by+8 <= h; by += 8 {
		for bx := 0; bx+8 <= w; bx += 8 {
			var m float64
			for y := 0; y < 8; y++ {
				for x := 0; x < 8; x++ {
					m += errs[(by+y)*w+bx+x]
				}
			}
			wins = append(wins, math.Abs(m/64))
		}
	}
	var sum float64
	for _, v := range wins {
		sum += v
	}
	mean := sum / float64(len(wins))
	// p95
	sorted := append([]float64(nil), wins...)
	sort.Float64s(sorted)
	return mean, sorted[len(sorted)*95/100]
}

func main() {
	files, _ := filepath.Glob("research/orginal/*.png")
	ties := []float64{0, 0.02, 0.05, 0.1, 0.2}

	fmt.Printf("%-30s %6s %10s %8s %8s %8s\n", "файл", "tie", "размер", "Δsize", "SSIM", "bias/p95")
	baseSizes := map[string]int64{}

	for _, tie := range ties {
		outDir := fmt.Sprintf("/tmp/exp_ent/tie_%.2f", tie)
		os.MkdirAll(outDir, 0o755)
		for _, f := range files {
			out := filepath.Join(outDir, filepath.Base(f))
			t0 := time.Now()
			res := compress.File(f, out, compress.Options{Dither: -1, Clamp: 0, EntropyTie: tie, Jobs: 1})
			if res.Err != nil {
				fmt.Printf("%-30s tie=%.2f ошибка: %v\n", filepath.Base(f), tie, res.Err)
				continue
			}
			_ = t0
			// метрики
			ofh, _ := os.Open(f)
			orig, _, _ := image.Decode(ofh)
			ofh.Close()
			nfh, _ := os.Open(out)
			dec, _, _ := image.Decode(nfh)
			nfh.Close()
			b := orig.Bounds()
			w, h := b.Dx(), b.Dy()
			ol, nl := luma(orig, w, h), luma(dec, w, h)
			s := ssim(ol, nl, w, h)
			bm, p95 := bias(orig, dec, w, h)

			name := filepath.Base(f)
			if len(name) > 29 {
				name = name[:29]
			}
			if tie == 0 {
				baseSizes[filepath.Base(f)] = res.After
			}
			base := baseSizes[filepath.Base(f)]
			d := 100 * (res.After - base) / base
			fmt.Printf("%-30s %6.2f %7dКБ %+7d%% %8.4f %.2f/%.2f\n",
				name, tie, res.After/1024, d, s, bm, p95)
		}
	}
}
