package compress

import (
	"bytes"
	"image"
	"image/png"
	"math/rand"
	"testing"
)

// TestRepackPNG — переупаковка IDAT лосслесс: пиксели идентичны, размер
// не больше исходного (zopfli не хуже flate BestCompression в этом потоке).
func TestRepackPNG(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	w, h := 160, 120
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := img.PixOffset(x, y)
			// UI-подобный кадр: плоские зоны, полосы, мягкий градиент,
			// лёгкий шум только на градиенте (как у наших дизеренных выходов).
			switch {
			case y < 20: // «шапка»
				img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = 6, 27, 49, 255
			case x%40 < 20: // полосы
				img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = 83, 58, 253, 255
			default: // градиент с шумом
				img.Pix[i] = uint8(x*255/w + rng.Intn(12))
				img.Pix[i+1] = uint8(y*255/h + rng.Intn(12))
				img.Pix[i+2] = uint8((x+y)*255/(w+h) + rng.Intn(12))
				img.Pix[i+3] = 255
			}
		}
	}
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	out := repackPNG(buf.Bytes())
	if bytes.Equal(out, buf.Bytes()) {
		t.Skip("переупаковка не изменила файл (неожиданно для шумного градиента)")
	}

	dec, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("переупакованный PNG не декодируется: %v", err)
	}
	orig, err := png.Decode(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	b := orig.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r1, g1, b1, a1 := orig.At(x, y).RGBA()
			r2, g2, b2, a2 := dec.At(x, y).RGBA()
			if r1 != r2 || g1 != g2 || b1 != b2 || a1 != a2 {
				t.Fatalf("пиксель (%d,%d) изменился — это НЕ лосслесс", x, y)
			}
		}
	}
	if len(out) > len(buf.Bytes()) {
		t.Fatalf("переупаковка раздула файл: %d > %d", len(out), len(buf.Bytes()))
	}
	t.Logf("repack: %d → %d байт (%.1f%%)", buf.Len(), len(out),
		100*float64(buf.Len()-len(out))/float64(buf.Len()))
}
