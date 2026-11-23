package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Candidate struct {
	Name   string  `json:"name"`
	Size   int64   `json:"size"`
	PSNR   float64 `json:"psnr"`
	SSIM   float64 `json:"ssim"`
	Colors int     `json:"colors"`
}

type PairReport struct {
	Name       string     `json:"name"`
	OrigSize   int64      `json:"orig_size"`
	CompSize   int64      `json:"comp_size"`
	Tiny       Metrics    `json:"tiny_metrics"`
	OursSize   int64      `json:"ours_size"`
	Ours       Metrics    `json:"ours_metrics"`
	GateSizeOK bool       `json:"gate_size_ok"` // размер ≤ +20% от TinyPNG
	GateSSIMOK bool       `json:"gate_ssim_ok"` // SSIM ≥ TinyPNG − 0.02
	PNGOrig    *PNGInfo   `json:"png_orig,omitempty"`
	PNGComp    *PNGInfo   `json:"png_comp,omitempty"`
	JPEGOrg    *JPEGInfo  `json:"jpeg_orig,omitempty"`
	JPEGComp   *JPEGInfo  `json:"jpeg_comp,omitempty"`
	PNGCands   []Candidate `json:"png_candidates,omitempty"`
	JPEGScan   []Candidate `json:"jpeg_scan,omitempty"`
	BestCand   string     `json:"best_candidate"`
}

func main() {
	origDir, compDir, oursDir := "orginal", "compressed_by_tinypng", "output"
	if len(os.Args) >= 3 {
		origDir, compDir = os.Args[1], os.Args[2]
	}
	if len(os.Args) >= 4 {
		oursDir = os.Args[3]
	}

	entries, err := os.ReadDir(origDir)
	if err != nil {
		fmt.Println("нет каталога оригиналов:", err)
		os.Exit(1)
	}

	var reports []PairReport
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		origPath := filepath.Join(origDir, e.Name())
		compPath := filepath.Join(compDir, e.Name())
		if _, err := os.Stat(compPath); err != nil {
			fmt.Println("пропуск (нет пары):", e.Name())
			continue
		}
		fmt.Println("жарим:", e.Name())
		rep, err := analyzePair(origPath, compPath, filepath.Join(oursDir, e.Name()))
		if err != nil {
			fmt.Println("  ошибка:", err)
			continue
		}
		reports = append(reports, *rep)
	}

	jsonBytes, err := json.MarshalIndent(reports, "", "  ")
	if err != nil {
		fmt.Println("ошибка сериализации JSON:", err)
		jsonBytes = []byte("[]")
	}
	safeWrite("report.json", jsonBytes)
	md := renderReport(reports)
	safeWrite("report.md", []byte(md))
	fmt.Println("\nготово: report.md, report.json")
}

// safeWrite — атомарная запись: краш не оставит обрезанный файл.
func safeWrite(path string, data []byte) {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		fmt.Println("ошибка записи:", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		fmt.Println("ошибка переименования:", err)
	}
}

func decode(path string) (image.Image, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	st, _ := f.Stat()
	img, _, err := image.Decode(f)
	return img, st.Size(), err
}

func analyzePair(origPath, compPath, oursPath string) (*PairReport, error) {
	origImg, origSize, err := decode(origPath)
	if err != nil {
		return nil, err
	}
	compImg, compSize, err := decode(compPath)
	if err != nil {
		return nil, err
	}
	rep := &PairReport{
		Name:     filepath.Base(origPath),
		OrigSize: origSize,
		CompSize: compSize,
		Tiny:     computeMetrics(origImg, compImg),
	}

	// Наш компрессор (если output/ собран) + гейты Q8/Q11.
	if oursImg, oursSize, err := decode(oursPath); err == nil {
		rep.OursSize = oursSize
		rep.Ours = computeMetrics(origImg, oursImg)
		rep.GateSizeOK = float64(oursSize) <= 1.2*float64(compSize)
		rep.GateSSIMOK = rep.Ours.SSIM >= rep.Tiny.SSIM-0.02
	}

	ext := strings.ToLower(filepath.Ext(origPath))
	switch ext {
	case ".png":
		rep.PNGOrig, _ = parsePNG(origPath)
		rep.PNGComp, _ = parsePNG(compPath)
		rep.PNGCands = pngCandidates(origImg)
		// Лучший кандидат: минимальный размер среди с PSNR >= TinyPNG PSNR (или все).
		best := -1
		for i, c := range rep.PNGCands {
			ok := c.PSNR+0.5 >= rep.Tiny.PSNR || math.IsInf(rep.Tiny.PSNR, 1)
			if ok && (best < 0 || c.Size < rep.PNGCands[best].Size) {
				best = i
			}
		}
		if best < 0 && len(rep.PNGCands) > 0 {
			best = 0
		}
		if best >= 0 {
			rep.BestCand = rep.PNGCands[best].Name
		}
	case ".jpg", ".jpeg":
		rep.JPEGOrg, _ = parseJPEG(origPath)
		rep.JPEGComp, _ = parseJPEG(compPath)
		rep.JPEGScan = jpegScan(origImg, compSize)
		sort.Slice(rep.JPEGScan, func(i, j int) bool { return rep.JPEGScan[i].Size < rep.JPEGScan[j].Size })
	}
	return rep, nil
}

func pngCandidates(orig image.Image) []Candidate {
	b := orig.Bounds()
	w, h := b.Dx(), b.Dy()
	get := func(x, y int) [4]uint8 { return toNRGBA(orig.At(b.Min.X+x, b.Min.Y+y)) }

	pixels := make([][4]uint8, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			pixels[y*w+x] = get(x, y)
		}
	}

	var out []Candidate
	for _, n := range []int{256, 128, 64} {
		pal := quantize(pixels, n)
		for _, dither := range []bool{true, false} {
			name := fmt.Sprintf("median-cut %d + zlib-best", n)
			if dither {
				name += " + FS-dither"
			} else {
				name += " (без дизеринга)"
			}
			img := buildPaletted(w, h, get, pal, dither)
			encoded, err := encodePNG(img)
			if err != nil {
				continue
			}
			m := computeMetrics(orig, img)
			out = append(out, Candidate{
				Name: name, Size: int64(len(encoded)),
				PSNR: round2(m.PSNR), SSIM: round2(m.SSIM), Colors: len(pal),
			})
		}
	}
	return out
}

func jpegScan(orig image.Image, tinySize int64) []Candidate {
	b := orig.Bounds()
	step := 1
	if b.Dx()*b.Dy() > 8_000_000 {
		step = 2 // большие JPEG: считаем по подвыборке
	}
	var out []Candidate
	for _, q := range []int{95, 90, 85, 80, 75, 70, 65, 60, 50} {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, orig, &jpeg.Options{Quality: q}); err != nil {
			continue
		}
		dec, _, err := image.Decode(bytes.NewReader(buf.Bytes()))
		if err != nil {
			continue
		}
		psnr, ssimVal := computeQuality(orig, dec, step)
		out = append(out, Candidate{
			Name: fmt.Sprintf("image/jpeg q=%d", q), Size: int64(buf.Len()),
			PSNR: round2(psnr), SSIM: round2(ssimVal),
		})
	}
	_ = tinySize
	return out
}

func round2(v float64) float64 {
	if math.IsInf(v, 1) {
		return v
	}
	return math.Round(v*100) / 100
}

func kb(n int64) string {
	return fmt.Sprintf("%.1f КБ", float64(n)/1024)
}

func mbps(n int64) string {
	return fmt.Sprintf("%.2f МБ", float64(n)/1024/1024)
}

func pct(orig, comp int64) string {
	if orig == 0 {
		return "—"
	}
	return fmt.Sprintf("−%.1f%%", 100-float64(comp)*100/float64(orig))
}

func fmtPSNR(v float64) string {
	if math.IsInf(v, 1) {
		return "∞ (без потерь)"
	}
	return fmt.Sprintf("%.2f дБ", v)
}

func renderReport(reports []PairReport) string {
	var b strings.Builder
	w := func(s string) { b.WriteString(s + "\n") }

	w("# Исследование сжатия изображений: TinyPNG vs алгоритм на Go")
	w("")
	w(fmt.Sprintf("_Сгенерировано: %s, инструмент: `research/tool` (чистый Go, stdlib)._", time.Now().Format("02.01.2006 15:04")))
	w("")
	w("Методика: каждая пара «оригинал → результат TinyPNG» разбирается на уровне формата")
	w("(PNG-чанки, JPEG-маркеры, квант-таблицы, фильтры строк), считаются метрики качества")
	w("(PSNR по RGB поверх белого, SSIM по яркости в окнах 8×8, уникальные цвета),")
	w("затем оригиналы прожариваются собственной Go-реализацией (median-cut квантизация +")
	w("опциональный Floyd–Steinberg + BestCompression) и сравниваются с TinyPNG по размеру и качеству.")
	w("")

	// ---- 1. Структура файлов ----
	w("## 1. Что именно делает TinyPNG с файлами")
	w("")
	w("### PNG")
	w("")
	w("| Файл | Цвет. модель до → после | Палитра (записей) | Метаданные | IDAT сжатие | Фильтры строк (N/S/U/A/P) |")
	w("|------|------------------------|-------------------|------------|-------------|---------------------------|")
	for _, r := range reports {
		if r.PNGOrig == nil {
			continue
		}
		meta := "сохранены"
		if len(r.PNGComp.TextChunks) == 0 && len(r.PNGOrig.TextChunks) > 0 {
			meta = fmt.Sprintf("удалены (%d)", len(r.PNGOrig.TextChunks))
		} else if len(r.PNGComp.TextChunks) == 0 {
			meta = "нет"
		}
		pal := "—"
		if r.PNGComp.PaletteMax > 0 {
			pal = fmt.Sprintf("%d", r.PNGComp.PaletteMax)
		}
		comp := "—"
		if r.PNGComp.IDATRaw > 0 {
			comp = fmt.Sprintf("%.2f%%", float64(r.PNGComp.IDATComp)*100/float64(r.PNGComp.IDATRaw))
		}
		fh := r.PNGComp.FilterHist
		w(fmt.Sprintf("| %s | %s → %s | %s | %s | %s | %d/%d/%d/%d/%d |",
			r.Name, r.PNGOrig.ColorTypeName, r.PNGComp.ColorTypeName, pal, meta, comp,
			fh[0], fh[1], fh[2], fh[3], fh[4]))
	}
	w("")
	w("Чанки TinyPNG-версии по файлам:")
	w("")
	for _, r := range reports {
		if r.PNGComp == nil {
			continue
		}
		var types []string
		for _, c := range r.PNGComp.Chunks {
			types = append(types, fmt.Sprintf("%s(%d)", c.Type, c.Size))
		}
		w(fmt.Sprintf("- `%s`: %s", r.Name, strings.Join(types, ", ")))
	}
	w("")
	w("### JPEG")
	w("")
	w("| Файл | Режим | Субдискретизация | ~качество (по квант-таблицам) | Маркеры после |")
	w("|------|-------|------------------|-------------------------------|---------------|")
	for _, r := range reports {
		if r.JPEGOrg == nil {
			continue
		}
		mode := "baseline"
		if r.JPEGComp.Progressive {
			mode = "progressive"
		}
		var markers []string
		for _, m := range r.JPEGComp.Markers {
			markers = append(markers, m.Type)
		}
		w(fmt.Sprintf("| %s | %s | %s | %d | %s |",
			r.Name, mode, r.JPEGComp.Sampling, r.JPEGComp.EstQuality, strings.Join(markers, ", ")))
	}
	w("")

	// ---- 2. Размеры и качество ----
	w("## 2. Размеры и качество результата TinyPNG")
	w("")
	w("| Файл | Было | Стало | Экономия | PSNR | SSIM | Уник. цветов до → после |")
	w("|------|------|-------|----------|------|------|-------------------------|")
	for _, r := range reports {
		w(fmt.Sprintf("| %s | %s | %s | %s | %s | %.4f | %d → %d |",
			r.Name, mbps(r.OrigSize), kb(r.CompSize), pct(r.OrigSize, r.CompSize),
			fmtPSNR(r.Tiny.PSNR), r.Tiny.SSIM, r.Tiny.UniqueOrig, r.Tiny.UniqueComp))
	}
	w("")

	// ---- 3. Go-кандидат vs TinyPNG ----
	w("## 3. Наш алгоритм на Go против TinyPNG")
	w("")
	w("Реализация: median-cut квантизация палитры (256/128/64 цветов),")
	w("дизеринг Флойда–Стейнберга (опционально), палитровый PNG, BestCompression, без метаданных.")
	w("")
	for _, r := range reports {
		if len(r.PNGCands) == 0 {
			continue
		}
		w(fmt.Sprintf("### %s (TinyPNG: %s, PSNR %s)", r.Name, kb(r.CompSize), fmtPSNR(r.Tiny.PSNR)))
		w("")
		w("| Вариант | Размер | Δ к TinyPNG | PSNR | SSIM |")
		w("|---------|--------|-------------|------|------|")
		for _, c := range r.PNGCands {
			delta := fmt.Sprintf("%+.1f%%", (float64(c.Size)-float64(r.CompSize))*100/float64(r.CompSize))
			w(fmt.Sprintf("| %s | %s | %s | %.2f дБ | %.4f |", c.Name, kb(c.Size), delta, c.PSNR, c.SSIM))
		}
		w("")
		w(fmt.Sprintf("_Лучший по критерию «размер при PSNR не хуже TinyPNG»: **%s**._", r.BestCand))
		w("")
	}
	for _, r := range reports {
		if len(r.JPEGScan) == 0 {
			continue
		}
		w(fmt.Sprintf("### %s (JPEG, TinyPNG: %s)", r.Name, kb(r.CompSize)))
		w("")
		w("Прогон качества stdlib-энкодера (baseline, без mozjpeg); для изображений >8 Мп метрики по подвыборке 2×:")
		w("")
		w("| Вариант | Размер | Δ к TinyPNG | PSNR | SSIM |")
		w("|---------|--------|-------------|------|------|")
		for _, c := range r.JPEGScan {
			delta := fmt.Sprintf("%+.1f%%", (float64(c.Size)-float64(r.CompSize))*100/float64(r.CompSize))
			w(fmt.Sprintf("| %s | %s | %s | %.2f дБ | %.4f |", c.Name, kb(c.Size), delta, c.PSNR, c.SSIM))
		}
		w("")
	}
	// ---- 4. Гейты ----
	w("## 4. Гейты: наш компрессор против TinyPNG")
	w("")
	w("Критерии (Q8/Q11): размер ≤ +20% от TinyPNG; SSIM ≥ TinyPNG − 0.02.")
	w("")
	w("| Файл | Наш размер | Δ к TinyPNG | Наш PSNR | Наш SSIM | TinyPNG SSIM | Гейт размера | Гейт SSIM |")
	w("|------|------------|-------------|----------|----------|--------------|--------------|-----------|")
	gatesOK := 0
	gatesTotal := 0
	for _, r := range reports {
		if r.OursSize == 0 {
			continue
		}
		gatesTotal++
		if r.GateSizeOK && r.GateSSIMOK {
			gatesOK++
		}
		verdictSize, verdictSSIM := "✓", "✓"
		if !r.GateSizeOK {
			verdictSize = "✗"
		}
		if !r.GateSSIMOK {
			verdictSSIM = "✗"
		}
		w(fmt.Sprintf("| %s | %s | %+.1f%% | %.2f дБ | %.4f | %.4f | %s | %s |",
			r.Name, kb(r.OursSize),
			(float64(r.OursSize)-float64(r.CompSize))*100/float64(r.CompSize),
			r.Ours.PSNR, r.Ours.SSIM, r.Tiny.SSIM, verdictSize, verdictSSIM))
	}
	w("")
	if gatesTotal > 0 {
		w(fmt.Sprintf("**Вердикт: %d/%d пар проходят оба гейта.**", gatesOK, gatesTotal))
	}
	w("")
	return b.String()
}
