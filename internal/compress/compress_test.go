package compress

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// gradientPNG — тестовая картинка: градиент + детерминированный шум,
// больше 256 цветов; шум ещё и делает источник «настоящим»
// (чистый градиент сам по себе жмётся в килобайт, и палитра его не обгонит).
func gradientPNG(w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	rng := rand.New(rand.NewSource(1))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			n := rng.Intn(48) - 24
			clamp := func(v int) uint8 {
				if v < 0 {
					return 0
				}
				if v > 255 {
					return 255
				}
				return uint8(v)
			}
			img.SetNRGBA(x, y, color.NRGBA{
				R: clamp(x*255/w + n),
				G: clamp(y*255/h + n),
				B: clamp((x+y)%256 + n),
				A: 255,
			})
		}
	}
	return img
}

func writePNG(t *testing.T, path string, img image.Image) {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestFilePNGGradient — палитровая стратегия, результат декодируется,
// размер меньше исходного, геометрия сохранена.
func TestFilePNGGradient(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "grad.png")
	dst := filepath.Join(dir, "out", "grad.png")
	writePNG(t, src, gradientPNG(256, 256))

	r := File(src, dst, Options{Dither: -1})
	if r.Err != nil {
		t.Fatalf("File: %v", r.Err)
	}
	if r.After >= r.Before {
		t.Fatalf("не сжалось: %d -> %d", r.Before, r.After)
	}
	f, err := os.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatalf("результат не декодируется: %v", err)
	}
	if img.Bounds().Dx() != 256 || img.Bounds().Dy() != 256 {
		t.Fatalf("геометрия поплыла: %v", img.Bounds())
	}
}

// TestFileJPEGSmallKept — маленькие JPEG не пережимаем (как TinyPNG).
func TestFileJPEGSmallKept(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "small.jpg")
	dst := filepath.Join(dir, "out", "small.jpg")

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, gradientPNG(80, 80), &jpeg.Options{Quality: 85}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	r := File(src, dst, Options{})
	if r.Err != nil {
		t.Fatalf("File: %v", r.Err)
	}
	if r.Strategy != "маленький — не трогаем" {
		t.Fatalf("стратегия: %q", r.Strategy)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, buf.Bytes()) {
		t.Fatal("маленький JPEG должен сохраняться байт-в-байт")
	}
}

// TestDir — пакетная обработка: все изображения найдены, порядок стабильный,
// не-изображения пропущены.
func TestDir(t *testing.T) {
	dir := t.TempDir()
	writePNG(t, filepath.Join(dir, "a.png"), gradientPNG(64, 64))
	writePNG(t, filepath.Join(dir, "b.png"), gradientPNG(64, 64))
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	results, err := Dir(dir, filepath.Join(dir, "out"), Options{Jobs: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("ожидалось 2 результата, получено %d", len(results))
	}
	if results[0].Name != "a.png" || results[1].Name != "b.png" {
		t.Fatalf("порядок нарушен: %s, %s", results[0].Name, results[1].Name)
	}
	for _, r := range results {
		if r.Err != nil {
			t.Fatalf("%s: %v", r.Name, r.Err)
		}
	}
}
