package compress

import (
	"math"
	"sort"
)

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

	// boxPopExp — показатель веса населения коробки при выборе сплита:
	// размах канала × население^0.25. 0 — классический median-cut (чистый
	// размах) скупится на широкие плотные градиенты (лесенка на icon.png),
	// 0.5 уже раздувает фото-PNG сверх гейта (Nyasha +20.6%). 0.25 подобран
	// по корпусу: bias icon 0.507→0.35, Nyasha остаётся в гейте (+4.2%).
	const boxPopExp = 0.25
	for len(boxes) < maxColors {
		bi, bestCh := -1, 0
		bestScore := 0.0
		for i, bx := range boxes {
			if bx.hi-bx.lo < 2 {
				continue
			}
			pop := float64(bx.hi - bx.lo)
			for c := 0; c < 4; c++ {
				r := float64(int(bx.maxC[c]) - int(bx.minC[c]))
				if s := r * math.Pow(pop, boxPopExp); s > bestScore {
					bestScore, bestCh, bi = s, c, i
				}
			}
		}
		if bi < 0 || bestScore == 0 {
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
//
// Ранняя остановка точная, а не по эпсилону: если назначения кластеров
// совпали с прошлой итерацией, то и центроиды пересчитаются в те же
// значения — дальнейшие итерации ничего не меняют (палитра на выходе
// идентична полному прогону, поведенческая нейтральность сохранена).
func lloydRefine(hist map[[4]uint8]int, pal [][4]uint8, iters int) [][4]uint8 {
	colors := make([][4]uint8, 0, len(hist))
	for c := range hist {
		colors = append(colors, c)
	}
	// Порядок обхода map случаен, а float-суммы не ассоциативны: без
	// сортировки центроиды округлялись в ±1 уровень от запуска к запуску
	// и выход одного файла гулял по байтам. Фиксированный порядок
	// накопления = детерминированная палитра.
	sort.Slice(colors, func(i, j int) bool {
		a, b := colors[i], colors[j]
		for k := 0; k < 4; k++ {
			if a[k] != b[k] {
				return a[k] < b[k]
			}
		}
		return false
	})
	weights := make([]int, len(colors))
	for i := range colors {
		weights[i] = hist[colors[i]]
	}
	pal = append([][4]uint8(nil), pal...)
	assign := make([]uint8, len(colors))
	prev := make([]uint8, len(colors))
	for it := 0; it < iters; it++ {
		pi := newPalIndex(pal) // палитра меняется — дерево перестраиваем
		sums := make([][4]float64, len(pal))
		counts := make([]float64, len(pal))
		for i, c := range colors {
			idx := int(pi.nearest(c))
			assign[i] = uint8(idx)
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
		// Палитра уже пересчитана в стабильное состояние: если назначения
		// не изменились, следующие итерации повторили бы её точно.
		if it > 0 && equalAssign(assign, prev) {
			break
		}
		assign, prev = prev, assign
	}
	return pal
}

func equalAssign(a, b []uint8) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

