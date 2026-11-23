package main

import (
	"encoding/json"
	"image"
	"image/color"
	"math"
)

// Metrics — попарные метрики качества между оригиналом и сжатой версией.
type Metrics struct {
	PSNR       float64 `json:"psnr"`   // дБ, по RGB поверх белого фона
	SSIM       float64 `json:"ssim"`   // по яркости, окна 8×8
	UniqueOrig int     `json:"unique_orig"`
	UniqueComp int     `json:"unique_comp"`
}

// MarshalJSON — +Inf (байт-в-байт идентичные файлы) в JSON не сериализуется,
// пишем null вместо падения всего отчёта.
func (m Metrics) MarshalJSON() ([]byte, error) {
	psnr := any(m.PSNR)
	if math.IsInf(m.PSNR, 0) || math.IsNaN(m.PSNR) {
		psnr = nil
	}
	return json.Marshal(struct {
		PSNR       any     `json:"psnr"`
		SSIM       float64 `json:"ssim"`
		UniqueOrig int     `json:"unique_orig"`
		UniqueComp int     `json:"unique_comp"`
	}{psnr, m.SSIM, m.UniqueOrig, m.UniqueComp})
}

// nrgbaOverWhite — NRGBA-пиксель, сведённый поверх белого фона.
func nrgbaOverWhite(img image.Image, x, y int) (r, g, b, a float64) {
	c := img.At(x, y)
	nr := toNRGBA(c)
	r, g, b, a = float64(nr[0])/255, float64(nr[1])/255, float64(nr[2])/255, float64(nr[3])/255
	return r*a + (1-a), g*a + (1-a), b*a + (1-a), a
}

func toNRGBA(c interface {
	RGBA() (uint32, uint32, uint32, uint32)
}) (out [4]uint8) {
	nc := color.NRGBAModel.Convert(c).(color.NRGBA)
	return [4]uint8{nc.R, nc.G, nc.B, nc.A}
}

// computeQuality — PSNR + SSIM без подсчёта уникальных цветов.
// step > 1 считает по подвыборке (для больших JPEG это в разы быстрее,
// метрики почти не расходятся).
func computeQuality(orig, comp image.Image, step int) (psnr, ssimVal float64) {
	b := orig.Bounds()
	w, h := b.Dx(), b.Dy()
	var mse float64
	var n int
	lumaO := make([]float64, 0, (w/step+1)*(h/step+1))
	lumaC := make([]float64, 0, (w/step+1)*(h/step+1))
	for y := 0; y < h; y += step {
		for x := 0; x < w; x += step {
			or, og, ob, _ := nrgbaOverWhite(orig, b.Min.X+x, b.Min.Y+y)
			cr, cg, cb, _ := nrgbaOverWhite(comp, b.Min.X+x, b.Min.Y+y)
			dr, dg, db := or-cr, og-cg, ob-cb
			mse += dr*dr + dg*dg + db*db
			n++
			lumaO = append(lumaO, 0.299*or+0.587*og+0.114*ob)
			lumaC = append(lumaC, 0.299*cr+0.587*cg+0.114*cb)
		}
	}
	mse /= float64(n * 3) / 3
	if mse == 0 {
		psnr = math.Inf(1)
	} else {
		psnr = 10 * math.Log10(1.0/mse)
	}
	// SSIM по «строкам подвыборки» — окна 8×8 на упакованной сетке.
	sw := (w + step - 1) / step
	ssimVal = ssim(lumaO, lumaC, sw, (n+sw-1)/sw)
	return psnr, ssimVal
}

func computeMetrics(orig, comp image.Image) Metrics {
	b := orig.Bounds()
	m := Metrics{}
	step := 1
	if b.Dx()*b.Dy() > 8_000_000 {
		step = 2
	}
	m.PSNR, m.SSIM = computeQuality(orig, comp, step)
	m.UniqueOrig = countUnique(orig, b)
	m.UniqueComp = countUnique(comp, b)
	return m
}

// ssim — средний SSIM по неперекрывающимся окнам 8×8.
func ssim(a, b []float64, w, h int) float64 {
	const win = 8
	const C1 = 0.01 * 0.01
	const C2 = 0.03 * 0.03
	n := win * win
	var total float64
	var windows int
	for wy := 0; wy+win <= h; wy += win {
		for wx := 0; wx+win <= w; wx += win {
			var ma, mb, va, vb, cov float64
			for y := 0; y < win; y++ {
				for x := 0; x < win; x++ {
					i := (wy+y)*w + wx + x
					ma += a[i]
					mb += b[i]
				}
			}
			ma /= float64(n)
			mb /= float64(n)
			for y := 0; y < win; y++ {
				for x := 0; x < win; x++ {
					i := (wy+y)*w + wx + x
					da := a[i] - ma
					db := b[i] - mb
					va += da * da
					vb += db * db
					cov += da * db
				}
			}
			va /= float64(n - 1)
			vb /= float64(n - 1)
			cov /= float64(n - 1)
			num := (2*ma*mb + C1) * (2*cov + C2)
			den := (ma*ma + mb*mb + C1) * (va + vb + C2)
			total += num / den
			windows++
		}
	}
	if windows == 0 {
		return 0
	}
	return total / float64(windows)
}

func countUnique(img image.Image, b image.Rectangle) int {
	step := 1
	if b.Dx()*b.Dy() > 4_000_000 {
		step = 2 // огромные JPEG: уникальные цветы по подвыборке
	}
	seen := make(map[[4]uint8]struct{})
	for y := b.Min.Y; y < b.Max.Y; y += step {
		for x := b.Min.X; x < b.Max.X; x += step {
			seen[toNRGBA(img.At(x, y))] = struct{}{}
		}
	}
	return len(seen)
}
