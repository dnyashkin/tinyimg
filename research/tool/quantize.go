package main

import (
	"image"
	"image/color"
	"image/png"
	"bytes"
	"sort"
)

// quantize — median-cut квантизация до maxColors цветов (палитра RGBA).
func quantize(pixels [][4]uint8, maxColors int) [][4]uint8 {
	type box struct{ lo, hi int } // полуинтервал индексов в срезе
	idx := make([]int, len(pixels))
	for i := range idx {
		idx[i] = i
	}
	boxes := []box{{0, len(idx)}}

	for len(boxes) < maxColors {
		// Ищем коробку с наибольшим диапазоном каналов.
		bi := -1
		bestRange := 0
		bestCh := 0
		for i, bx := range boxes {
			if bx.hi-bx.lo < 2 {
				continue
			}
			var minC, maxC [4]uint8
			for c := 0; c < 4; c++ {
				minC[c], maxC[c] = 255, 0
			}
			for j := bx.lo; j < bx.hi; j++ {
				p := pixels[idx[j]]
				for c := 0; c < 4; c++ {
					if p[c] < minC[c] {
						minC[c] = p[c]
					}
					if p[c] > maxC[c] {
						maxC[c] = p[c]
					}
				}
			}
			for c := 0; c < 4; c++ {
				if r := int(maxC[c]) - int(minC[c]); r > bestRange {
					bestRange, bestCh, bi = r, c, i
				}
			}
		}
		if bi < 0 || bestRange == 0 {
			break
		}
		// Делим коробку по медиане выбранного канала.
		bx := boxes[bi]
		ch := bestCh
		sort.Slice(idx[bx.lo:bx.hi], func(i, j int) bool {
			return pixels[idx[bx.lo+i]][ch] < pixels[idx[bx.lo+j]][ch]
		})
		mid := (bx.lo + bx.hi) / 2
		boxes[bi] = box{bx.lo, mid}
		boxes = append(boxes, box{mid, bx.hi})
	}

	pal := make([][4]uint8, 0, len(boxes))
	for _, bx := range boxes {
		var sum [4]int
		for j := bx.lo; j < bx.hi; j++ {
			p := pixels[idx[j]]
			for c := 0; c < 4; c++ {
				sum[c] += int(p[c])
			}
		}
		n := bx.hi - bx.lo
		if n == 0 {
			continue
		}
		var avg [4]uint8
		for c := 0; c < 4; c++ {
			avg[c] = uint8(sum[c] / n)
		}
		pal = append(pal, avg)
	}
	return pal
}

func sqDist(a, b [4]uint8) int {
	d := 0
	for c := 0; c < 4; c++ {
		dd := int(a[c]) - int(b[c])
		d += dd * dd
	}
	return d
}

func nearest(pal [][4]uint8, p [4]uint8, cache map[[4]uint8]uint8) uint8 {
	if i, ok := cache[p]; ok {
		return i
	}
	best, bestD := 0, 1<<30
	for i, q := range pal {
		if d := sqDist(p, q); d < bestD {
			best, bestD = i, d
		}
	}
	cache[p] = uint8(best)
	return uint8(best)
}

// buildPaletted — палитровое изображение из пикселей; dither включает Floyd–Steinberg.
func buildPaletted(w, h int, get func(x, y int) [4]uint8, pal [][4]uint8, dither bool) *image.Paletted {
	palCol := make(color.Palette, len(pal))
	for i, p := range pal {
		// color.NRGBA — прямые (не premultiplied) значения, как в get().
		palCol[i] = color.NRGBA{p[0], p[1], p[2], p[3]}
	}
	img := image.NewPaletted(image.Rect(0, 0, w, h), palCol)

	if !dither {
		cache := make(map[[4]uint8]uint8)
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				img.SetColorIndex(x, y, nearest(pal, get(x, y), cache))
			}
		}
		return img
	}

	// Floyd–Steinberg: диффузия ошибки квантования в RGBA.
	buf := make([][4]float64, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			p := get(x, y)
			for c := 0; c < 4; c++ {
				buf[y*w+x][c] = float64(p[c])
			}
		}
	}
	cache := make(map[[4]uint8]uint8)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := y*w + x
			var cur [4]uint8
			for c := 0; c < 4; c++ {
				v := buf[i][c]
				if v < 0 {
					v = 0
				} else if v > 255 {
					v = 255
				}
				cur[c] = uint8(v + 0.5)
			}
			idx := nearest(pal, cur, cache)
			img.SetColorIndex(x, y, idx)
			var err [4]float64
			for c := 0; c < 4; c++ {
				err[c] = buf[i][c] - float64(pal[idx][c])
			}
			diffuse := func(xx, yy int, f float64) {
				if xx < 0 || xx >= w || yy >= h {
					return
				}
				t := &buf[yy*w+xx]
				for c := 0; c < 4; c++ {
					t[c] += err[c] * f
				}
			}
			diffuse(x+1, y, 7.0/16)
			diffuse(x-1, y+1, 3.0/16)
			diffuse(x, y+1, 5.0/16)
			diffuse(x+1, y+1, 1.0/16)
		}
	}
	return img
}

// encodePNG — палитровый PNG с максимальным сжатием DEFLATE.
func encodePNG(img *image.Paletted) ([]byte, error) {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
