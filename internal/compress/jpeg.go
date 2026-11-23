package compress

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"os"
)

// Цель пережатия: stdlib-качество, сопоставимое с TinyPNG.
// По исследованию (research/report.md) их результаты ложатся в зону q50–60;
// q=52 даёт запас по гейту размера (+20% к TinyPNG) без заметной потери.
const jpegTargetQuality = 52

// Маленькие JPEG не пережимаем: экономия не стоит риска для качества
// (TinyPNG так же оставил 65-килобайтный 2.jpg нетронутым).
const jpegMinSize = 100 * 1024

// compressJPEG — пережатие JPEG в духе TinyPNG: примерно постоянное
// качество q≈50 (stdlib-шкала; TinyPNG по исследованию сидит в зоне q50–60),
// метаданные вырезаются. Если выгода < 10% — оставляем оригинал как есть
// (TinyPNG так же не трогает уже оптимальные файлы).
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

	out := encodeJPEG(orig, jpegTargetQuality)
	if out == nil {
		return 0, 0, "", fmt.Errorf("кодирование не удалось")
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
	return before, int64(len(out)), fmt.Sprintf("JPEG q=%d (зона TinyPNG)", jpegTargetQuality), nil
}

func encodeJPEG(img image.Image, q int) []byte {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: q}); err != nil {
		return nil
	}
	return buf.Bytes()
}
