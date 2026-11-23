package compress

import (
	"math"
	"sort"
)

// Веса каналов в метрике близости цвета: зелёный глаз заметнее всего
// (как в libimagequant). Единственный источник правды — sqDist тоже их использует.
var kdWeights = [4]int{3, 6, 1, 2}

// percLUT — перцептуальное кодирование канала: cbrt (≈ яркость Lab).
// В сыром sRGB шаг кода в тёмной зоне (10→11) выглядит в разы сильнее,
// чем в светлой (200→201); без этого палитра жадничает цвета из тёмных
// градиентов, и там же вылезает «лесенка» (проверено на тёмно-синих артах).
var percLUT [256]float64

// percInvLUT — обратное преобразование (среднее в перцептуальном пространстве
// → код sRGB).
var percInvLUT [512]uint8

func init() {
	for i := range percLUT {
		percLUT[i] = math.Cbrt(float64(i)/255) * 255
	}
	for i := range percInvLUT {
		v := float64(i) / 511 // 0..1 с шагом 1/511 (среднее может быть дробным)
		percInvLUT[i] = uint8(math.Round(math.Pow(v, 3) * 255))
	}
}

func toPerc(c uint8) float64 { return percLUT[c] }

// fromPerc — перцептуальное значение (0..255) → код sRGB.
func fromPerc(p float64) uint8 {
	i := int(p / 255 * 511)
	if i < 0 {
		i = 0
	} else if i > 511 {
		i = 511
	}
	return percInvLUT[i]
}

// sqDist — взвешенное квадратичное расстояние по RGBA в перцептуальном
// пространстве (см. percLUT).
func sqDist(a, b [4]uint8) int {
	dr := toPerc(a[0]) - toPerc(b[0])
	dg := toPerc(a[1]) - toPerc(b[1])
	db := toPerc(a[2]) - toPerc(b[2])
	da := float64(a[3]) - float64(b[3]) // альфа — сырая
	w := kdWeights
	return int(float64(w[0])*dr*dr + float64(w[1])*dg*dg + float64(w[2])*db*db + float64(w[3])*da*da)
}

// kdNode — узел k-d дерева: палитра живёт в 4-мерном RGBA-пространстве.
type kdNode struct {
	pt          [4]uint8
	idx         uint8
	axis        uint8
	left, right *kdNode
}

// buildKD — построение дерева по палитре: ось с наибольшим взвешенным
// разбросом, медиана по ней. Для 256 точек построение стоит копеек,
// зато поиск ближайшего цвета становится O(log n) вместо O(256).
func buildKD(pal [][4]uint8) *kdNode {
	idxs := make([]int, len(pal))
	for i := range idxs {
		idxs[i] = i
	}
	return buildKDRec(pal, idxs)
}

func buildKDRec(pal [][4]uint8, idxs []int) *kdNode {
	if len(idxs) == 0 {
		return nil
	}
	axis, bestSpread := 0, -1.0
	for a := 0; a < 4; a++ {
		mn, mx := 255.0, 0.0
		for _, i := range idxs {
			v := toPerc(pal[i][a])
			if a == 3 {
				v = float64(pal[i][3])
			}
			if v < mn {
				mn = v
			}
			if v > mx {
				mx = v
			}
		}
		if spread := (mx - mn) * float64(kdWeights[a]); spread > bestSpread {
			bestSpread, axis = spread, a
		}
	}
	sort.Slice(idxs, func(i, j int) bool {
		return pal[idxs[i]][axis] < pal[idxs[j]][axis]
	})
	mid := len(idxs) / 2
	n := &kdNode{pt: pal[idxs[mid]], idx: uint8(idxs[mid]), axis: uint8(axis)}
	n.left = buildKDRec(pal, idxs[:mid])
	n.right = buildKDRec(pal, idxs[mid+1:])
	return n
}

// query — точный ближайший сосед. Tie-break как у перебора: при равных
// расстояниях побеждает меньший индекс палитры. Отсечение ветки только
// при plane > bestD гарантирует, что равные по расстоянию точки не выпадут.
func (n *kdNode) query(p [4]uint8, bestIdx uint8, bestD int) (uint8, int) {
	if n == nil {
		return bestIdx, bestD
	}
	if d := sqDist(p, n.pt); d < bestD || (d == bestD && n.idx < bestIdx) {
		bestIdx, bestD = n.idx, d
	}
	diff := toPerc(p[n.axis]) - toPerc(n.pt[n.axis])
	if n.axis == 3 {
		diff = float64(p[3]) - float64(n.pt[3])
	}
	plane := int(float64(kdWeights[n.axis]) * diff * diff)
	near, far := n.left, n.right
	if diff >= 0 {
		near, far = n.right, n.left
	}
	bestIdx, bestD = near.query(p, bestIdx, bestD)
	if plane <= bestD {
		bestIdx, bestD = far.query(p, bestIdx, bestD)
	}
	return bestIdx, bestD
}

// palIndex — быстрый поиск по палитре: k-d дерево + кэш повторных цветов.
type palIndex struct {
	root  *kdNode
	cache map[[4]uint8]uint8
}

func newPalIndex(pal [][4]uint8) *palIndex {
	return &palIndex{root: buildKD(pal), cache: make(map[[4]uint8]uint8, 1<<14)}
}

func (pi *palIndex) nearest(p [4]uint8) uint8 {
	if i, ok := pi.cache[p]; ok {
		return i
	}
	i, _ := pi.root.query(p, 0, 1<<30)
	pi.cache[p] = i
	return i
}
