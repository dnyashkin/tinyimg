package compress

// medianCut — квантизация палитры: делим облако цветов по медиане канала
// с наибольшим диапазоном, цвет коробки — среднее её пикселей.
// Разбиение — поразрядной сортировкой по каналу (O(n+256) вместо O(n log n)):
// на многомегапиксельных PNG это основная экономия времени.
func medianCut(pixels [][4]uint8, maxColors int) [][4]uint8 {
	type box struct {
		lo, hi     int
		minC, maxC [4]uint8 // статистика коробки — считается при разбиении,
		// чтобы не сканировать все пиксели на каждой итерации (O(n×255) — убийца)
	}
	idx := make([]int, len(pixels))
	for i := range idx {
		idx[i] = i
	}
	// Статистика коробки: один проход по её пикселям.
	boxStats := func(lo, hi int) (minC, maxC [4]uint8) {
		for c := 0; c < 4; c++ {
			minC[c], maxC[c] = 255, 0
		}
		for j := lo; j < hi; j++ {
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
		return minC, maxC
	}
	tmp := make([]int, len(pixels))
	lo0, max0 := boxStats(0, len(idx))
	boxes := []box{{0, len(idx), lo0, max0}}

	for len(boxes) < maxColors {
		// Ищем коробку с наибольшим диапазоном — по сохранённой статистике.
		bi, bestRange, bestCh := -1, 0, 0
		for i, bx := range boxes {
			if bx.hi-bx.lo < 2 {
				continue
			}
			for c := 0; c < 4; c++ {
				if r := int(bx.maxC[c]) - int(bx.minC[c]); r > bestRange {
					bestRange, bestCh, bi = r, c, i
				}
			}
		}
		if bi < 0 || bestRange == 0 {
			break
		}
		// Делим коробку по медиане выбранного канала: устойчивая
		// поразрядная сортировка подотрезка, порядок дублей сохраняется.
		bx := boxes[bi]
		ch := bestCh
		var start [256]int
		for j := bx.lo; j < bx.hi; j++ {
			start[pixels[idx[j]][ch]]++
		}
		acc := 0
		for v := 0; v < 256; v++ {
			c := start[v]
			start[v] = acc
			acc += c
		}
		for j := bx.lo; j < bx.hi; j++ {
			v := pixels[idx[j]][ch]
			tmp[start[v]] = idx[j]
			start[v]++
		}
		copy(idx[bx.lo:bx.hi], tmp[:bx.hi-bx.lo])
		mid := (bx.lo + bx.hi) / 2
		lmin, lmax := boxStats(bx.lo, mid)
		rmin, rmax := boxStats(mid, bx.hi)
		boxes[bi] = box{bx.lo, mid, lmin, lmax}
		boxes = append(boxes, box{mid, bx.hi, rmin, rmax})
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

// lloydRefine — несколько итераций Ллойда: двигаем цвета палитры к центроидам
// своих кластеров (по гистограмме уникальных цветов с весами), ошибка
// квантования падает, SSIM растёт. Размер почти не меняется.
func lloydRefine(pixels [][4]uint8, pal [][4]uint8, iters int) [][4]uint8 {
	hist := make(map[[4]uint8]int, 1<<14)
	for _, p := range pixels {
		hist[p]++
	}
	colors := make([][4]uint8, 0, len(hist))
	weights := make([]int, 0, len(hist))
	for c, n := range hist {
		colors = append(colors, c)
		weights = append(weights, n)
	}
	pal = append([][4]uint8(nil), pal...)
	for it := 0; it < iters; it++ {
		pi := newPalIndex(pal) // палитра меняется — дерево перестраиваем
		sums := make([][4]float64, len(pal))
		counts := make([]float64, len(pal))
		for i, c := range colors {
			idx := int(pi.nearest(c))
			w := float64(weights[i])
			// Считаем в перцептуальном пространстве (см. kd.go) —
			// среднее в сыром sRGB размывает тёмные ступени.
			sums[idx][0] += toPerc(c[0]) * w
			sums[idx][1] += toPerc(c[1]) * w
			sums[idx][2] += toPerc(c[2]) * w
			sums[idx][3] += float64(c[3]) * w
			counts[idx] += w
		}
		for j := range pal {
			if counts[j] == 0 {
				continue
			}
			pal[j][0] = fromPerc(sums[j][0] / counts[j])
			pal[j][1] = fromPerc(sums[j][1] / counts[j])
			pal[j][2] = fromPerc(sums[j][2] / counts[j])
			pal[j][3] = uint8(sums[j][3]/counts[j] + 0.5)
		}
	}
	return pal
}
