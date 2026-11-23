package compress

// Тесты защит: лимит пикселей (против «бомб распаковки») и 16-битный
// lossless-путь PNG.

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hugeIHDR — минимальный PNG с огромными размерами в заголовке:
// сигнатура + IHDR + IEND. Декод такого не нужен — гард обязан сработать
// на DecodeConfig, до выделения памяти.
func hugeIHDR(w, h uint32) []byte {
	var out bytes.Buffer
	out.Write([]byte{137, 80, 78, 71, 13, 10, 26, 10})
	var ihdr [13]byte
	binary.BigEndian.PutUint32(ihdr[0:4], w)
	binary.BigEndian.PutUint32(ihdr[4:8], h)
	ihdr[8] = 8 // bit depth
	ihdr[9] = 6 // RGBA
	writePNGChunk(&out, "IHDR", ihdr[:])
	writePNGChunk(&out, "IEND", nil)
	return out.Bytes()
}

// TestPNGTooLarge — «бомба распаковки» отклоняется до декода пикселей.
func TestPNGTooLarge(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "bomb.png")
	if err := os.WriteFile(src, hugeIHDR(12000, 12000), 0o644); err != nil {
		t.Fatal(err)
	}
	r := File(src, filepath.Join(dir, "out.png"), Options{})
	if r.Err == nil || !strings.Contains(r.Err.Error(), "слишком большие размеры") {
		t.Fatalf("ожидалась ошибка размеров, получили: %v", r.Err)
	}
	// Граница лимита: ровно MaxPixels должно проходить проверку.
	if err := checkDims(10000, 10000, "x"); err != nil {
		t.Fatalf("ровно 100 Мпикс должно проходить: %v", err)
	}
	if err := checkDims(10001, 10000, "x"); err == nil {
		t.Fatal("100 Мпикс + 1 должно отклоняться")
	}
}

// TestJPEGTooLarge — то же для JPEG: патчим SOF0 у крошечного файла.
func TestJPEGTooLarge(t *testing.T) {
	var buf bytes.Buffer
	img := image.NewNRGBA(image.Rect(0, 0, 16, 16))
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	// SOF0 (0xFFC0): marker, 2 байта длины, 1 байт точности, 2 высоты, 2 ширины.
	patched := false
	for i := 0; i+9 < len(data); i++ {
		if data[i] == 0xFF && data[i+1] == 0xC0 {
			binary.BigEndian.PutUint16(data[i+5:i+7], 16384)
			binary.BigEndian.PutUint16(data[i+7:i+9], 16384)
			patched = true
			break
		}
	}
	if !patched {
		t.Fatal("SOF0 не найден в выводе jpeg.Encode")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "bomb.jpg")
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatal(err)
	}
	r := File(src, filepath.Join(dir, "out.jpg"), Options{})
	if r.Err == nil || !strings.Contains(r.Err.Error(), "слишком большие размеры") {
		t.Fatalf("ожидалась ошибка размеров, получили: %v", r.Err)
	}
}

// TestPNG16BitStaysLossless — 16-битный PNG не уходит в 8-битную палитру:
// глубина и пиксели сохраняются, стратегия честно называется lossless.
func TestPNG16BitStaysLossless(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "deep.png")
	dst := filepath.Join(dir, "out.png")

	img := image.NewNRGBA64(image.Rect(0, 0, 64, 64))
	c1 := color.NRGBA64{R: 0x1234, G: 0x5678, B: 0x9ABC, A: 0xFFFF}
	c2 := color.NRGBA64{R: 0xDEF0, G: 0x1357, B: 0x9BDF, A: 0xFFFF}
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			if (x+y)%2 == 0 {
				img.SetNRGBA64(x, y, c1)
			} else {
				img.SetNRGBA64(x, y, c2)
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	r := File(src, dst, Options{Dither: -1, Squeeze: true})
	if r.Err != nil {
		t.Fatalf("File: %v", r.Err)
	}
	if !strings.Contains(r.Strategy, "16-бит") {
		t.Fatalf("стратегия должна помечать 16-битный путь: %q", r.Strategy)
	}
	out, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("результат не декодируется: %v", err)
	}
	// Оба 16-битных типа — сохранение глубины (stdlib отдаёт RGBA64 для
	// непрозрачного NRGBA64: альфа-канал не пишется, глубина та же).
	var is16bit bool
	switch dec.(type) {
	case *image.NRGBA64, *image.RGBA64:
		is16bit = true
	}
	if !is16bit {
		t.Fatalf("глубина потеряна: результат %T вместо 16-битного", dec)
	}
	// Пиксели — точь-в-точь: никакой скрытой потери глубины.
	// Шахматка: чётная сумма координат — c1, нечётная — c2.
	checks := []struct {
		x, y int
		c    color.NRGBA64
	}{{0, 0, c1}, {1, 0, c2}, {63, 63, c1}}
	for _, chk := range checks {
		got := color.NRGBA64Model.Convert(dec.At(chk.x, chk.y)).(color.NRGBA64)
		if got != chk.c {
			t.Fatalf("пиксель (%d,%d): %v != %v", chk.x, chk.y, got, chk.c)
		}
	}
}
