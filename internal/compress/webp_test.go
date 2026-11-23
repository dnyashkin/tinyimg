//go:build cgo

package compress

// Тесты webp-пайплайна (только с cgo/libwebp): round-trip кодека и
// стратегии сжатия — lossless-переупак и lossy-адаптивный q.

import (
	"bytes"
	"encoding/binary"
	"image"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func webpTestImage(w, h int) *image.NRGBA {
	rng := rand.New(rand.NewSource(42))
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := img.PixOffset(x, y)
			img.Pix[i] = uint8(x*255/w + rng.Intn(20))
			img.Pix[i+1] = uint8(y*255/h + rng.Intn(20))
			img.Pix[i+2] = uint8((x + y))
			img.Pix[i+3] = 255
		}
	}
	return img
}

func TestWebPRoundTrip(t *testing.T) {
	img := webpTestImage(320, 240)
	pix := img.Pix
	lossy := webpEncodeLossy(320, 240, pix, 80)
	if len(lossy) == 0 {
		t.Fatal("lossy-энкод не сработал")
	}
	w, h, rgba, err := webpDecode(lossy)
	if err != nil || w != 320 || h != 240 || len(rgba) != w*h*4 {
		t.Fatalf("декод: %dx%d len=%d err=%v", w, h, len(rgba), err)
	}
	if webpIsLossless(lossy) {
		t.Fatal("lossy-выход не должен помечаться lossless")
	}
	lossless := webpEncodeLossless(320, 240, pix)
	if len(lossless) == 0 {
		t.Fatal("lossless-энкод не сработал")
	}
	if !webpIsLossless(lossless) {
		t.Fatal("lossless-выход должен помечаться lossless")
	}
	_, _, rl, err := webpDecode(lossless)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rl, pix) {
		t.Fatal("lossless webp не сохранил пиксели")
	}
}

func TestCompressWebPStrategies(t *testing.T) {
	dir := t.TempDir()

	// Lossy-вход: больший q → пережимается при SSIM ≥ 0.98.
	img := webpTestImage(640, 480)
	src := filepath.Join(dir, "in.webp")
	if err := os.WriteFile(src, webpEncodeLossy(640, 480, img.Pix, 92), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "out.webp")
	before, after, strategy, err := compressWebP(src, dst, Options{}, newStageLogger(Options{}))
	if err != nil {
		t.Fatal(err)
	}
	if after <= 0 || before <= 0 {
		t.Fatalf("размеры: %d → %d", before, after)
	}
	t.Logf("lossy: %d → %d (%s)", before, after, strategy)
	if after < before && strategy == "webp q (уже оптимален)" {
		t.Fatalf("стратегия не соответствует результату: %s", strategy)
	}

	// Lossless-вход: переупак или «уже оптимален», но файл валиден.
	src2 := filepath.Join(dir, "in2.webp")
	if err := os.WriteFile(src2, webpEncodeLossless(640, 480, img.Pix), 0o644); err != nil {
		t.Fatal(err)
	}
	dst2 := filepath.Join(dir, "out2.webp")
	_, _, strategy2, err := compressWebP(src2, dst2, Options{}, newStageLogger(Options{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("lossless: %s", strategy2)
	out, err := os.ReadFile(dst2)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := webpDecode(out); err != nil {
		t.Fatalf("выходной webp не декодируется: %v", err)
	}
}

// TestCompressFileWebPDispatch — .webp уходит в webp-пайплайн из compressFile.
func TestCompressFileWebPDispatch(t *testing.T) {
	dir := t.TempDir()
	img := webpTestImage(64, 64)
	src := filepath.Join(dir, "a.webp")
	if err := os.WriteFile(src, webpEncodeLossy(64, 64, img.Pix, 80), 0o644); err != nil {
		t.Fatal(err)
	}
	res := File(src, filepath.Join(dir, "a.out.webp"), Options{})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if res.Strategy == "" {
		t.Fatal("пустая стратегия")
	}
}

// webpAlphaImage — картинка с прозрачной половиной: RGB под альфой не
// определён, и метрика качества обязана это учитывать.
func webpAlphaImage(w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := img.PixOffset(x, y)
			if x < w/2 {
				img.Pix[i], img.Pix[i+1], img.Pix[i+2] = 20, 90, 200
				img.Pix[i+3] = 255
			} else {
				img.Pix[i], img.Pix[i+1], img.Pix[i+2] = 200, 30, 60
				img.Pix[i+3] = 0 // полностью прозрачно
			}
		}
	}
	return img
}

// TestWebPAlphaPreserved — прозрачность webp не теряется при сжатии:
// альфа-канал выхода совпадает с исходным, а прозрачная половина не
// «съедается» метрикой качества.
func TestWebPAlphaPreserved(t *testing.T) {
	dir := t.TempDir()
	const w, h = 128, 64
	img := webpAlphaImage(w, h)
	src := filepath.Join(dir, "alpha.webp")
	if err := os.WriteFile(src, webpEncodeLossless(w, h, img.Pix), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "alpha.out.webp")
	before, after, strategy, err := compressWebP(src, dst, Options{}, newStageLogger(Options{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("alpha: %d → %d (%s)", before, after, strategy)
	out, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	ow, oh, rgba, err := webpDecode(out)
	if err != nil || ow != w || oh != h {
		t.Fatalf("декод выхода: %dx%d err=%v", ow, oh, err)
	}
	for i := 0; i < w*h; i++ {
		if rgba[i*4+3] != img.Pix[i*4+3] {
			t.Fatalf("альфа разошлась в пикселе %d: %d != %d", i, rgba[i*4+3], img.Pix[i*4+3])
		}
	}
}

// TestRGBSSIMSkipsTransparent — прозрачные пиксели не должны ронять SSIM:
// у прозрачного пикселя RGB не определён, поэтому сравнивать его нельзя.
func TestRGBSSIMSkipsTransparent(t *testing.T) {
	const w, h = 16, 16
	a := make([]uint8, w*h*4)
	b := make([]uint8, w*h*4)
	for i := 0; i < w*h; i++ {
		a[i*4], a[i*4+1], a[i*4+2], a[i*4+3] = 10, 20, 30, 255
		b[i*4], b[i*4+1], b[i*4+2], b[i*4+3] = 10, 20, 30, 255
	}
	// Прозрачная половина исходника: RGB под альфой не определён, поэтому
	// сравнивать его нельзя (у кандидата этот пиксель даже непрозрачный —
	// расхождение альфы ловит отдельный тест TestWebPAlphaPreserved).
	for i := w * h / 2; i < w*h; i++ {
		a[i*4], a[i*4+1], a[i*4+2] = 0, 0, 0
		a[i*4+3] = 0
		b[i*4], b[i*4+1], b[i*4+2] = 255, 255, 255
		b[i*4+3] = 255
	}
	if got := rgbSSIM(a, b, w, h); got < 0.999 {
		t.Fatalf("прозрачные пиксели повлияли на SSIM: %f", got)
	}
}

// TestRGBSSIMCatchesChroma — лумовый SSIM слеп к замене цвета с той же
// яркостью (красный ↔ зелёный подобранной силы): именно такую деградацию
// даёт YUV420 на резких цветных краях. rgbSSIM обязан её ловить.
func TestRGBSSIMCatchesChroma(t *testing.T) {
	const w, h = 16, 16
	mk := func(r, g, b uint8) []uint8 {
		buf := make([]uint8, w*h*4)
		for i := 0; i < w*h; i++ {
			buf[i*4], buf[i*4+1], buf[i*4+2], buf[i*4+3] = r, g, b, 255
		}
		return buf
	}
	// Лума (255,0,0) = 76.245 ≈ луме (0,130,0) = 76.31: лумовая метрика
	// дала бы ~1.0, хромовая — заметно меньше.
	a, b := mk(255, 0, 0), mk(0, 130, 0)
	if got := rgbSSIM(a, b, w, h); got >= webpSSIMGate {
		t.Fatalf("замена хромы при той же луме не поймана: SSIM=%f", got)
	}
	if got := rgbSSIM(a, a, w, h); got < 0.999 {
		t.Fatalf("идентичные кадры: SSIM=%f", got)
	}
}

// TestWebPTooLarge — огромный canvas в VP8X отклоняется до декода:
// гард по пикселям срабатывает на заголовке, память не выделяется.
func TestWebPTooLarge(t *testing.T) {
	var b bytes.Buffer
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(4+8+10))
	b.WriteString("WEBP")
	b.WriteString("VP8X")
	binary.Write(&b, binary.LittleEndian, uint32(10))
	b.WriteByte(0)           // флаги
	b.Write([]byte{0, 0, 0}) // зарезервировано
	put24 := func(v uint32) { b.Write([]byte{byte(v), byte(v >> 8), byte(v >> 16)}) }
	put24(12000 - 1) // canvas 12000×12000 = 144 Мпикс > лимита 100 Мпикс
	put24(12000 - 1)

	dir := t.TempDir()
	src := filepath.Join(dir, "huge.webp")
	if err := os.WriteFile(src, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := compressWebP(src, filepath.Join(dir, "out.webp"), Options{}, newStageLogger(Options{}))
	if err == nil || !strings.Contains(err.Error(), "слишком большие размеры") {
		t.Fatalf("ожидалась ошибка размеров, получили: %v", err)
	}
}
