package compress

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"os"
)

// Цель пережатия: stdlib-качество, сопоставимое с TinyPNG.
// По исследованию (research/report.md) их результаты ложатся в зону q50–60.
const jpegTargetQuality = 52

// Маленькие JPEG не пережимаем: экономия не стоит риска для качества
// (TinyPNG так же оставил 65-килобайтный 2.jpg нетронутым).
const jpegMinSize = 100 * 1024

// jpegSSIMGate — гейт качества по SSIM поверх даунсэмпла 2×. Даунсэмпл
// усредняет пиксельный шум (он и так невидим и прячет артефакты), поэтому
// метрика реагирует именно на потерю резкости и структуры — то, что видит
// глаз. Проверено на жалобах: полноразмерный SSIM на шумных фото никогда
// не доходит до 0.97 (метрика слепнет в шуме), а на 2× жалобный файл
// отделяется с запасом (0.86 против ≥0.94 у остальных).
const jpegSSIMGate = 0.98

// jpegLadder — лестница качества: берём наименьший q, проходящий гейт.
var jpegLadder = []int{52, 58, 65, 72, 80, 88}

// compressJPEG — пережатие JPEG с адаптивным качеством: наименьший q из
// лестницы, у которого SSIM (на 2×-даунсэмпле) к оригиналу ≥ 0.98. Если ни
// один q не проходит гейт или экономия < 10% — оставляем оригинал как есть:
// уже плотный JPEG (мелкая текстура, резкая хрома) stdlib-энкодером
// (только 4:2:0) не улучшить без видимой потери резкости, и честный ответ
// для такого файла — «не трогаем».
func compressJPEG(src, dst string) (before, after int64, strategy string, err error) {
	raw, err := os.ReadFile(src)
	if err != nil {
		return 0, 0, "", err
	}
	before = int64(len(raw))
	// Заголовок до всего остального: «бомба распаковки» (огромные размеры
	// в SOF при крошечном файле — сплошная заливка жмётся в килобайты)
	// отсекается до выделения памяти под пиксели.
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return 0, 0, "", fmt.Errorf("декод: %w", err)
	}
	if err := checkDims(cfg.Width, cfg.Height, src); err != nil {
		return 0, 0, "", err
	}
	if before < jpegMinSize {
		// Маленький файл — не рискуем качеством ради копеек.
		if err := writeOut(dst, raw); err != nil {
			return 0, 0, "", err
		}
		return before, before, "маленький — не трогаем", nil
	}
	orig, err := jpeg.Decode(bytes.NewReader(raw))
	if err != nil {
		return 0, 0, "", fmt.Errorf("декод: %w", err)
	}

	b := orig.Bounds()
	w, h := b.Dx(), b.Dy()
	origPix := imageToRGBA(orig)
	orig2x, w2, h2 := downsample2x(origPix, w, h)

	var out []byte
	bestQ := 0
	for _, q := range jpegLadder {
		cand := encodeJPEG(orig, q)
		if cand == nil {
			return 0, 0, "", fmt.Errorf("кодирование не удалось")
		}
		if int64(len(cand)) >= before {
			continue
		}
		dec, err := jpeg.Decode(bytes.NewReader(cand))
		if err != nil {
			continue
		}
		candPix := imageToRGBA(dec)
		cand2x, _, _ := downsample2x(candPix, w, h)
		if rgbSSIM(orig2x, cand2x, w2, h2) >= jpegSSIMGate {
			out, bestQ = cand, q
			break // наименьший проходящий q — максимум экономии
		}
	}
	if out == nil {
		// Ни один q не прошёл гейт: файл уже плотный, пережатие без
		// видимой потери невозможно — не портим.
		if err := writeOut(dst, raw); err != nil {
			return 0, 0, "", err
		}
		return before, before, "уже оптимален (не трогаем)", nil
	}
	if int64(len(out)) > before*9/10 {
		// Выгода мизерная — не портим файл, пишем как есть.
		if err := writeOut(dst, raw); err != nil {
			return 0, 0, "", err
		}
		return before, before, "уже оптимален (не трогаем)", nil
	}
	if err := writeOut(dst, out); err != nil {
		return 0, 0, "", err
	}
	return before, int64(len(out)), fmt.Sprintf("JPEG q=%d (SSIM2x ≥ %.2f)", bestQ, jpegSSIMGate), nil
}

// imageToRGBA — пиксели изображения в прямой RGBA-буфер (не premultiplied).
func imageToRGBA(img image.Image) []uint8 {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	pix := make([]uint8, w*h*4)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, bl, a := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			i := (y*w + x) * 4
			pix[i], pix[i+1], pix[i+2], pix[i+3] = uint8(r>>8), uint8(g>>8), uint8(bl>>8), uint8(a>>8)
		}
	}
	return pix
}

// downsample2x — усреднение блоками 2×2: пиксельный шум гасится, структура
// и резкость сохраняются — гейт качества меряется на этом представлении.
func downsample2x(p []uint8, w, h int) ([]uint8, int, int) {
	w2, h2 := w/2, h/2
	if w2 == 0 || h2 == 0 {
		return p, w, h
	}
	out := make([]uint8, w2*h2*4)
	for y := 0; y < h2; y++ {
		for x := 0; x < w2; x++ {
			var acc [4]int
			for dy := 0; dy < 2; dy++ {
				for dx := 0; dx < 2; dx++ {
					i := ((2*y+dy)*w + 2*x + dx) * 4
					for c := 0; c < 4; c++ {
						acc[c] += int(p[i+c])
					}
				}
			}
			o := (y*w2 + x) * 4
			for c := 0; c < 4; c++ {
				out[o+c] = uint8(acc[c] / 4)
			}
		}
	}
	return out, w2, h2
}

func encodeJPEG(img image.Image, q int) []byte {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: q}); err != nil {
		return nil
	}
	return buf.Bytes()
}
