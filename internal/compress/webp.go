//go:build cgo

package compress

// WebP через libwebp (cgo) — единственный внешний кодек проекта: в чистом
// Go webp не существует ни в декод, ни в энкод (стдlib + x/image дают лишь
// частичный декод). Решение принято осознанно (2026-11): webp→webp с
// качеством libwebp важнее нулевых зависимостей. Сборка без cgo подменяется
// заглушкой webp_nocgo.go.
//
// Стратегия: lossless-вход → сравниваем lossless-переупак (без потерь) и
// lossy-свип (SSIM ≥ 0.98) и берём меньший; lossy-вход → адаптивный q при
// SSIM ≥ 0.98 к пикселям входа и экономии ≥10%, иначе файл не трогаем.

/*
#cgo pkg-config: libwebp
#include <webp/decode.h>
#include <webp/encode.h>

// webp_encode_lossless_go — lossless через расширенный API (method=6,
// quality=100 — «effort»): плотнее дефолтного WebPEncodeLosslessRGBA.
// Владение буфером переходит вызывающему (освободить WebPFree).
static size_t webp_encode_lossless_go(const uint8_t* rgba, int w, int h,
                                      int stride, uint8_t** out) {
  WebPConfig cfg;
  WebPPicture pic;
  WebPMemoryWriter wrt;
  if (!WebPConfigInit(&cfg)) return 0;
  cfg.lossless = 1;
  cfg.quality = 100;
  cfg.method = 6;
  if (!WebPPictureInit(&pic)) return 0;
  pic.use_argb = 1;
  pic.width = w;
  pic.height = h;
  WebPMemoryWriterInit(&wrt);
  pic.writer = WebPMemoryWrite;
  pic.custom_ptr = &wrt;
  if (!WebPPictureImportRGBA(&pic, rgba, stride) || !WebPEncode(&cfg, &pic)) {
    WebPMemoryWriterClear(&wrt);
    WebPPictureFree(&pic);
    return 0;
  }
  *out = wrt.mem;
  size_t sz = wrt.size;
  WebPPictureFree(&pic);
  return sz;
}
*/
import "C"

import (
	"errors"
	"fmt"
	"os"
	"time"
	"unsafe"
)

// WebPSupported — доступен ли webp-кодек в этой сборке (нужен cgo + libwebp).
func WebPSupported() bool { return true }

// webpIsLossless — битстрим lossless (VP8L) или lossy (VP8)?
func webpIsLossless(data []byte) bool {
	var f C.WebPBitstreamFeatures
	if C.WebPGetFeatures((*C.uint8_t)(unsafe.Pointer(&data[0])), C.size_t(len(data)), &f) != C.VP8_STATUS_OK {
		return false
	}
	return f.format == 2 // 1 = lossy, 2 = lossless
}

// webpDims — размеры из заголовка без декода: гард против «бомб
// распаковки» срабатывает до выделения памяти под пиксели.
func webpDims(data []byte) (w, h int, err error) {
	if len(data) == 0 {
		return 0, 0, errors.New("webp: пустой файл")
	}
	var f C.WebPBitstreamFeatures
	if C.WebPGetFeatures((*C.uint8_t)(unsafe.Pointer(&data[0])), C.size_t(len(data)), &f) != C.VP8_STATUS_OK {
		return 0, 0, errors.New("libwebp: не удалось прочитать заголовок webp")
	}
	return int(f.width), int(f.height), nil
}

// webpDecode — декод в прямые RGBA (не premultiplied).
func webpDecode(data []byte) (w, h int, pix []uint8, err error) {
	if len(data) == 0 {
		return 0, 0, nil, errors.New("webp: пустой файл")
	}
	var ww, hh C.int
	out := C.WebPDecodeRGBA((*C.uint8_t)(unsafe.Pointer(&data[0])), C.size_t(len(data)), &ww, &hh)
	if out == nil {
		return 0, 0, nil, errors.New("libwebp: не удалось декодировать webp")
	}
	w, h = int(ww), int(hh)
	pix = C.GoBytes(unsafe.Pointer(out), C.int(w*h*4))
	// Буфер выделен libwebp — освобождаем через WebPFree (контракт API),
	// а не free: с кастомным аллокатором free был бы UB.
	C.WebPFree(unsafe.Pointer(out))
	return w, h, pix, nil
}

func webpEncodeLossy(w, h int, rgba []uint8, q float32) []byte {
	var out *C.uint8_t
	// Порядок: (данные, width, height, stride, quality, out) — stride ПОСЛЕ
	// размеров (libwebp не следует привычному stride-вторым; иначе тихий 0).
	sz := C.WebPEncodeRGBA((*C.uint8_t)(unsafe.Pointer(&rgba[0])), C.int(w), C.int(h), C.int(w*4), C.float(q), &out)
	if sz == 0 || out == nil {
		return nil
	}
	b := C.GoBytes(unsafe.Pointer(out), C.int(sz))
	C.WebPFree(unsafe.Pointer(out))
	return b
}

func webpEncodeLossless(w, h int, rgba []uint8) []byte {
	var out *C.uint8_t
	sz := C.webp_encode_lossless_go((*C.uint8_t)(unsafe.Pointer(&rgba[0])), C.int(w), C.int(h), C.int(w*4), &out)
	if sz == 0 || out == nil {
		return nil
	}
	b := C.GoBytes(unsafe.Pointer(out), C.int(sz))
	C.WebPFree(unsafe.Pointer(out))
	return b
}

// rgbSSIM — SSIM (окна 8×8) по каналам R, G, B двух RGBA-буферов,
// сведённый с весами яркости (0.299/0.587/0.114).
//
// Считать только луму нельзя: lossy-кодек с YUV420 размывает именно
// хрому (резкие цветные края, красный текст), а лумовый SSIM этого не
// видит — гейт пропускал бы заметную глазу деградацию.
//
// Прозрачные пиксели (альфа = 0) не сравниваются: RGB под полностью
// прозрачными пикселями не определён (энкодер их обнуляет или заполняет
// мусором), и без исключения метрика врёт на прозрачных картинках.
func rgbSSIM(a, b []uint8, w, h int) float64 {
	// Каналы разложены в float64-плоскости один раз; дальше оконный проход.
	n := w * h
	ca := make([][3]float64, n)
	cb := make([][3]float64, n)
	alpha := make([]uint8, n)
	for i := 0; i < n; i++ {
		ca[i] = [3]float64{float64(a[i*4]), float64(a[i*4+1]), float64(a[i*4+2])}
		cb[i] = [3]float64{float64(b[i*4]), float64(b[i*4+1]), float64(b[i*4+2])}
		// Прозрачен хотя бы в одном — RGB не определён, не сравниваем.
		if a[i*4+3] != 0 && b[i*4+3] != 0 {
			alpha[i] = 255
		}
	}
	const c1, c2 = 6.5025, 58.5225
	var total [3]float64
	var nWin int
	for by := 0; by+8 <= h; by += 8 {
		for bx := 0; bx+8 <= w; bx += 8 {
			var ma, mb, va, vb, cov [3]float64
			var cnt int
			for y := 0; y < 8; y++ {
				for x := 0; x < 8; x++ {
					idx := (by+y)*w + bx + x
					// сравниваем только непрозрачные пиксели
					if alpha[idx] == 0 {
						continue
					}
					pa, pb := ca[idx], cb[idx]
					for c := 0; c < 3; c++ {
						ma[c] += pa[c]
						mb[c] += pb[c]
						va[c] += pa[c] * pa[c]
						vb[c] += pb[c] * pb[c]
						cov[c] += pa[c] * pb[c]
					}
					cnt++
				}
			}
			if cnt == 0 {
				continue // окно целиком прозрачное — не оцениваем
			}
			f := float64(cnt)
			for c := 0; c < 3; c++ {
				ma[c] /= f
				mb[c] /= f
				va[c] = va[c]/f - ma[c]*ma[c]
				vb[c] = vb[c]/f - mb[c]*mb[c]
				cov[c] = cov[c]/f - ma[c]*mb[c]
				total[c] += ((2*ma[c]*mb[c] + c1) * (2*cov[c] + c2)) /
					((ma[c]*ma[c] + mb[c]*mb[c] + c1) * (va[c] + vb[c] + c2))
			}
			nWin++
		}
	}
	if nWin == 0 {
		return 1
	}
	// Веса яркости: итог чувствителен к хроме пропорционально её заметности.
	return (0.299*total[0] + 0.587*total[1] + 0.114*total[2]) / float64(nWin)
}

const webpSSIMGate = 0.98

// compressWebP — пайплайн webp. Lossy-вход: адаптивный q при SSIM ≥ 0.98
// и экономии ≥10%. Lossless-вход: сравниваем два маршрута и берём меньший —
// lossless-переупак (пиксель-в-пиксель) или lossy-свип по тому же контракту
// «визуально без потери», что и палитра для PNG: сохранивший lossless файл
// часто пережимается в разы без видимой разницы (проверено глазами на арте).
func compressWebP(src, dst string, opts Options, stage *stageLogger) (before, after int64, strategy string, err error) {
	raw, err := os.ReadFile(src)
	if err != nil {
		return 0, 0, "", err
	}
	before = int64(len(raw))
	// Заголовок до декода: размеры проверяем до выделения памяти под пиксели.
	dw, dh, err := webpDims(raw)
	if err != nil {
		return 0, 0, "", fmt.Errorf("декод: %w", err)
	}
	if err := checkDims(dw, dh, src); err != nil {
		return 0, 0, "", err
	}
	t0 := time.Now()
	w, h, rgba, err := webpDecode(raw)
	if err != nil {
		return 0, 0, "", fmt.Errorf("декод: %w", err)
	}
	stage.log("webp-decode", t0)

	lossless := webpIsLossless(raw)

	// Кандидат 1: lossless-переупак (без потерь совсем).
	var losslessOut []byte
	if lossless {
		t1 := time.Now()
		losslessOut = webpEncodeLossless(w, h, rgba)
		stage.log("webp-encode-lossless", t1)
		if len(losslessOut) >= len(raw) {
			losslessOut = nil
		}
	}

	// Кандидат 2: lossy-свип при SSIM ≥ 0.98 к пикселям входа.
	t2 := time.Now()
	var lossyOut []byte
	bestQ := 0
	for _, q := range []int{80, 72, 65, 58, 52, 46} {
		out := webpEncodeLossy(w, h, rgba, float32(q))
		if len(out) == 0 || len(out) >= len(raw) {
			continue
		}
		ow, oh, opix, derr := webpDecode(out)
		if derr != nil || ow != w || oh != h {
			continue
		}
		if rgbSSIM(rgba, opix, w, h) >= webpSSIMGate {
			lossyOut, bestQ = out, q
		}
	}
	stage.log("webp-encode-sweep", t2)

	// Экономия <10% от lossy — не рискуем генерацией (правило как у JPEG).
	if lossyOut != nil && float64(len(lossyOut)) > 0.9*float64(len(raw)) {
		lossyOut = nil
	}

	// Выбор: минимальный из кандидатов.
	out, strategy := raw, "webp q (уже оптимален)"
	switch {
	case losslessOut != nil && (lossyOut == nil || len(losslessOut) <= len(lossyOut)):
		out, strategy = losslessOut, "webp lossless (переупак)"
	case lossyOut != nil:
		out = lossyOut
		if lossless {
			strategy = fmt.Sprintf("webp lossless → q%d (SSIM ≥ %.2f)", bestQ, webpSSIMGate)
		} else {
			strategy = fmt.Sprintf("webp q%d (SSIM ≥ %.2f)", bestQ, webpSSIMGate)
		}
	case lossless:
		strategy = "webp lossless (уже оптимален)"
	}

	if err := writeOut(dst, out); err != nil {
		return 0, 0, "", err
	}
	return before, int64(len(out)), strategy, nil
}
