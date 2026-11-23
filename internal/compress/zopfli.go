// Package-внутренний zopfli-класс DEFLATE-энкодер (RFC 1951), stdlib-only.
// LZ77 (хэш-цепочки + кэш «ярусов» матчей) → squeeze (DP min-cost, 6 итераций
// с переоценкой модели стоимости) → динамические таблицы Хаффмана
// (package-merge, лимит 15 бит, Kraft-проверка) + RLE длин кодов.
// Лосслесс-переупаковка IDAT: против compress/flate BestCompression даёт
// −6…−7% на дизеренных палитровых данных (замер: research/exp_ent, 2026-11).
// Стоит в ~25 раз дороже базового flate — включать осознанно (Options.Squeeze).
package compress

import (
	"encoding/binary"
	"fmt"
	"sort"
)

// ---------------------------------------------------------------------------
// Adler-32 (RFC 1950), кусочками по 5552 байта как в zlib (NMAX).
// ---------------------------------------------------------------------------

func adler32(data []byte) uint32 {
	const mod = 65521
	var a, b uint32 = 1, 0
	for i := 0; i < len(data); i += 5552 {
		end := i + 5552
		if end > len(data) {
			end = len(data)
		}
		for _, c := range data[i:end] {
			a += uint32(c)
			b += a
		}
		a %= mod
		b %= mod
	}
	return b<<16 | a
}

// ---------------------------------------------------------------------------
// Битовый писатель: биты пакуются в байты LSB-first (RFC 1951).
// ---------------------------------------------------------------------------

type bitWriter struct {
	buf []byte
	acc uint64
	n   uint
}

// writeBits пишет младшие nb бит val, начиная с самого младшего (LSB-first).
// Так же пишутся extra-биты длин/дистанций.
func (w *bitWriter) writeBits(val uint32, nb uint) {
	if nb == 0 {
		return
	}
	var mask uint64 = (1 << nb) - 1
	w.acc |= (uint64(val) & mask) << w.n
	w.n += nb
	for w.n >= 8 {
		w.buf = append(w.buf, byte(w.acc))
		w.acc >>= 8
		w.n -= 8
	}
}

// writeHuff пишет код Хаффмана начиная со старшего бита кода (RFC 1951:
// «Huffman codes are packed starting with the most-significant bit»).
func (w *bitWriter) writeHuff(code uint32, length uint8) {
	for i := int(length) - 1; i >= 0; i-- {
		w.writeBits((code>>uint(i))&1, 1)
	}
}

func (w *bitWriter) finish() []byte {
	if w.n > 0 {
		w.buf = append(w.buf, byte(w.acc))
		w.acc, w.n = 0, 0
	}
	return w.buf
}

// ---------------------------------------------------------------------------
// Таблицы длин/дистанций RFC 1951 §3.2.5.
// ---------------------------------------------------------------------------

var lenBase = [29]int{3, 4, 5, 6, 7, 8, 9, 10, 11, 13, 15, 17, 19, 23, 27, 31, 35, 43, 51, 59, 67, 83, 99, 115, 131, 163, 195, 227, 258}
var lenExtra = [29]int{0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 1, 1, 2, 2, 2, 2, 3, 3, 3, 3, 4, 4, 4, 4, 5, 5, 5, 5, 0}

var distBase = [30]int{1, 2, 3, 4, 5, 7, 9, 13, 17, 25, 33, 49, 65, 97, 129, 193, 257, 385, 513, 769, 1025, 1537, 2049, 3073, 4097, 6145, 8193, 12289, 16385, 24577}
var distExtra = [30]int{0, 0, 0, 0, 1, 1, 2, 2, 3, 3, 4, 4, 5, 5, 6, 6, 7, 7, 8, 8, 9, 9, 10, 10, 11, 11, 12, 12, 13, 13}

// lenToSym: длина 3..258 -> символ lit/len (257..285).
var lenToSym [259]uint16

// distToSym: дистанция 1..32768 -> символ дистанции (0..29).
var distToSym [32769]uint8

// symBounds: длины, на которых меняется символ/число extra-бит длины
// (начала и концы диапазонов) — кандидаты DP, где «стоимость длин» скачет.
var symBounds []int

func init() {
	for s := 0; s < 29; s++ {
		lo := lenBase[s]
		hi := lo + (1 << lenExtra[s])
		if hi > 259 {
			hi = 259
		}
		for l := lo; l < hi; l++ {
			lenToSym[l] = uint16(257 + s)
		}
	}
	for s := 0; s < 30; s++ {
		lo := distBase[s]
		hi := lo + (1 << distExtra[s])
		for d := lo; d < hi && d <= winSize; d++ {
			distToSym[d] = uint8(s)
		}
	}
	for l := minMatch; l <= maxMatch; l++ {
		prev := uint16(0)
		if l > minMatch {
			prev = lenToSym[l-1]
		}
		next := uint16(0)
		if l < maxMatch {
			next = lenToSym[l+1]
		}
		if lenToSym[l] != prev || lenToSym[l] != next {
			symBounds = append(symBounds, l)
		}
	}
}

// ---------------------------------------------------------------------------
// Huffman: package-merge с ограничением длины кода (maxBits бит).
// ---------------------------------------------------------------------------

// pmNode: элемент слоя package-merge. leaf=true -> лист (idx = индекс символа),
// иначе пакет p: пара элементов (2p, 2p+1) предыдущего слоя.
type pmNode struct {
	w    uint64
	leaf bool
	idx  int
}

func huffmanLengths(freqs []uint64, maxBits int) ([]uint8, error) {
	lengths := make([]uint8, len(freqs))
	var leaves []pmNode
	for i, f := range freqs {
		if f > 0 {
			leaves = append(leaves, pmNode{w: f, leaf: true, idx: i})
		}
	}
	m := len(leaves)
	if m == 0 {
		return lengths, nil
	}
	if m == 1 {
		lengths[leaves[0].idx] = 1
		return lengths, nil
	}
	sort.Slice(leaves, func(i, j int) bool {
		if leaves[i].w != leaves[j].w {
			return leaves[i].w < leaves[j].w
		}
		return leaves[i].idx < leaves[j].idx
	})

	sel := 2*m - 2 // сколько узлов берём из последнего слоя

	// Слои: layer[0] = листья; layer[k] = merge(leafs, попарные суммы
	// layer[k-1]), усечённые до sel элементов (хвост не нужен).
	layers := make([][]pmNode, 0, maxBits)
	layers = append(layers, leaves)
	for k := 1; k < maxBits; k++ {
		prev := layers[k-1]
		pkgs := make([]pmNode, 0, len(prev)/2)
		for i := 0; i+1 < len(prev); i += 2 {
			pkgs = append(pkgs, pmNode{w: prev[i].w + prev[i+1].w, idx: i / 2})
		}
		merged := make([]pmNode, 0, sel)
		i, j := 0, 0
		for len(merged) < sel && (i < m || j < len(pkgs)) {
			if j >= len(pkgs) || (i < m && leaves[i].w <= pkgs[j].w) {
				merged = append(merged, leaves[i])
				i++
			} else {
				merged = append(merged, pkgs[j])
				j++
			}
		}
		layers = append(layers, merged)
	}

	// Раскатываем выделенные узлы последнего слоя вниз до листьев.
	counts := make([][]int, maxBits)
	for k := 0; k < maxBits; k++ {
		counts[k] = make([]int, len(layers[k]))
	}
	for i := 0; i < sel && i < len(counts[maxBits-1]); i++ {
		counts[maxBits-1][i] = 1
	}
	leafCount := make([]int, len(freqs)) // по индексам символов
	for k := maxBits - 1; k >= 1; k-- {
		cur := counts[k]
		below := counts[k-1]
		for j, c := range cur {
			if c == 0 {
				continue
			}
			node := layers[k][j]
			if node.leaf {
				leafCount[node.idx] += c
			} else {
				p := node.idx
				if 2*p+1 < len(below) {
					below[2*p] += c
					below[2*p+1] += c
				}
			}
		}
	}
	for j, c := range counts[0] {
		if c > 0 && layers[0][j].leaf {
			leafCount[layers[0][j].idx] += c
		}
	}
	for i := 0; i < m; i++ {
		lengths[leaves[i].idx] = uint8(leafCount[leaves[i].idx])
	}

	// Проверка Крафта: sum 2^-len == 1 для m >= 2. Это «не может случиться»,
	// но библиотечный код не паникует: зовущий repackPNG вернёт исходные
	// байты, как и при любой другой ошибке разбора.
	kraft := 0
	for _, l := range lengths {
		if l > 0 {
			kraft += 1 << uint(maxBits-int(l))
		}
	}
	if kraft != 1<<uint(maxBits) {
		return nil, fmt.Errorf("huffmanLengths: нарушено неравенство Крафта (kraft=%d, maxBits=%d)", kraft, maxBits)
	}
	return lengths, nil
}

// canonicalCodes: канонические коды RFC 1951 §3.2.2.
func canonicalCodes(lengths []uint8) []uint32 {
	var blCount [16]uint32
	var maxLen uint8
	for _, l := range lengths {
		if l > 0 {
			blCount[l]++
			if l > maxLen {
				maxLen = l
			}
		}
	}
	var nextCode [16]uint32
	code := uint32(0)
	for bits := uint8(1); bits <= maxLen; bits++ {
		code = (code + blCount[bits-1]) << 1
		nextCode[bits] = code
	}
	codes := make([]uint32, len(lengths))
	for i, l := range lengths {
		if l > 0 {
			codes[i] = nextCode[l]
			nextCode[l]++
		}
	}
	return codes
}

// ---------------------------------------------------------------------------
// LZ77: хэш-цепочки как в zlib (head/prev), перебор всех совпадений.
// ---------------------------------------------------------------------------

const (
	hashBits   = 15
	hashMask   = (1 << hashBits) - 1
	minMatch   = 3
	maxMatch   = 258
	winSize    = 32768
	chainMax   = 1024 // ограничение длины цепочки на позицию (скорость/качество)
	maxTierCnt = 16   // максимум «ярусов» на позицию (память кэша матчей)
)

func hash3(a, b, c byte) uint32 {
	return ((uint32(a) << 10) ^ (uint32(b) << 5) ^ uint32(c)) & hashMask
}

// tier: «ярус» матчей позиции — (верхняя длина ltop, дистанция d).
// Для любой длины k в (предыдущий ltop, ltop] пара (k, d) — валидный матч
// (k байт из матча длины ltop на дистанции d).
type tier struct {
	ltop uint16
	dist uint16
}

// matchCache: все матчи, найденные один раз на позицию; итерации squeeze
// переиспользуют кэш (матчи не зависят от модели стоимости).
type matchCache struct {
	off  []uint32 // off[i]..off[i+1] — ярусы позиции i в tiers
	tier []tier
}

type lz77 struct {
	data []byte
	head []int32
	prev []int32
}

func newLZ77(data []byte) *lz77 {
	z := &lz77{
		data: data,
		head: make([]int32, 1<<hashBits),
		prev: make([]int32, len(data)),
	}
	for i := range z.head {
		z.head[i] = -1
	}
	for i := range z.prev {
		z.prev[i] = -1
	}
	return z
}

// insert: вставка позиции i в хэш-цепочку (вызывается после запроса на i,
// поэтому цепочка всегда содержит только позиции < i).
func (z *lz77) insert(i int) {
	if i+minMatch > len(z.data) {
		return
	}
	h := hash3(z.data[i], z.data[i+1], z.data[i+2])
	z.prev[i] = z.head[h]
	z.head[h] = int32(i)
}

// matchLen: длина общего префикса data[i:] и data[j:] (не более max байт),
// по 8 байт за сравнение.
func matchLen(data []byte, i, j, max int) int {
	l := 0
	for l+8 <= max {
		if binary.LittleEndian.Uint64(data[i+l:]) != binary.LittleEndian.Uint64(data[j+l:]) {
			break
		}
		l += 8
	}
	for l < max && data[i+l] == data[j+l] {
		l++
	}
	return l
}

// buildCache: один проход по всем позициям; для каждой — перебор цепочки
// и запись ярусов (первый по дистанции матч, покрывающий длину k, и т.д.).
func (z *lz77) buildCache() *matchCache {
	data := z.data
	n := len(data)
	mc := &matchCache{
		off:  make([]uint32, n+1),
		tier: make([]tier, 0, n/2),
	}
	for i := 0; i < n; i++ {
		mc.off[i] = uint32(len(mc.tier))
		if i+minMatch <= n {
			maxLen := n - i
			if maxLen > maxMatch {
				maxLen = maxMatch
			}
			h := hash3(data[i], data[i+1], data[i+2])
			filled := 2 // максимальная длина, для которой ярус уже известен
			cnt := 0
			// Цепочка содержит только позиции < i (вставка — после запроса).
			for j := int(z.head[h]); j >= 0; j = int(z.prev[j]) {
				cnt++
				// Адаптивный бюджет цепочки: чем длиннее уже найденный матч,
				// тем меньше глубокий поиск добавляет качества.
				limit := chainMax
				if filled >= 128 {
					limit = 32
				} else if filled >= 48 {
					limit = 256
				}
				if cnt > limit {
					break
				}
				d := i - j
				if d > winSize {
					break // цепочка по убыванию j: дальше дистанции только больше
				}
				// Проба: если байт на смещении filled не совпадает, матч не может
				// быть длиннее filled и новых ярусов не даст — пропуск за 1 сравнение.
				if filled < maxLen && data[i+filled] != data[j+filled] {
					continue
				}
				l := matchLen(data, i, j, maxLen)
				if l < minMatch || l <= filled {
					continue // не добавляет новых длин; дальний матч ничего не улучшит
				}
				mc.tier = append(mc.tier, tier{ltop: uint16(l), dist: uint16(d)})
				filled = l
				if filled >= maxLen {
					break // длиннее не бывает, улучшить нечего
				}
				if len(mc.tier)-int(mc.off[i]) >= maxTierCnt {
					break // лимит ярусов на позицию (память)
				}
			}
		}
		z.insert(i)
	}
	mc.off[n] = uint32(len(mc.tier))
	return mc
}

// ---------------------------------------------------------------------------
// Модель стоимости (в битах) из длин кодов Хаффмана прошлого прохода.
// ---------------------------------------------------------------------------

type costModel struct {
	lit  [256]float64
	ll   [maxMatch + 1]float64 // по длине матча
	dist [winSize + 1]float64
	huge float64
}

func newCostModel(litlenLen, distLen []uint8) *costModel {
	cm := &costModel{huge: 1e18}
	for b := 0; b < 256; b++ {
		if litlenLen[b] > 0 {
			cm.lit[b] = float64(litlenLen[b])
		} else {
			cm.lit[b] = cm.huge
		}
	}
	for l := minMatch; l <= maxMatch; l++ {
		s := lenToSym[l]
		if litlenLen[s] > 0 {
			cm.ll[l] = float64(litlenLen[s]) + float64(lenExtra[s-257])
		} else {
			cm.ll[l] = cm.huge
		}
	}
	for d := 1; d <= winSize; d++ {
		s := distToSym[d]
		if distLen[s] > 0 {
			cm.dist[d] = float64(distLen[s]) + float64(distExtra[s])
		} else {
			cm.dist[d] = cm.huge
		}
	}
	return cm
}

// ---------------------------------------------------------------------------
// Токены: length == 0 -> литерал (байт берётся из data по позиции),
// иначе матч (length, dist).
// ---------------------------------------------------------------------------

type token struct {
	length uint16
	dist   uint16
}

// ---------------------------------------------------------------------------
// Squeeze: DP по позициям, min-cost path с моделью стоимости прошлого прохода.
// Кандидаты на позиции: вершины ярусов + границы символов длин (symBounds).
// ---------------------------------------------------------------------------

func squeeze(data []byte, mc *matchCache, cm *costModel) []token {
	n := len(data)
	cost := make([]float64, n+1)
	bestLen := make([]uint16, n)
	bestDist := make([]uint16, n)

	cost[n] = 0
	for i := n - 1; i >= 0; i-- {
		best := cm.lit[data[i]] + cost[i+1]
		bl, bd := uint16(0), uint16(0)

		a, b := mc.off[i], mc.off[i+1]
		bi := 0 // указатель в symBounds (они отсортированы по возрастанию)
		prevTop := minMatch - 1
		for t := a; t < b; t++ {
			ltop := int(mc.tier[t].ltop)
			d := mc.tier[t].dist
			// границы символов длин внутри яруса (prevTop, ltop]
			for bi < len(symBounds) && symBounds[bi] <= ltop {
				k := symBounds[bi]
				if k > prevTop {
					c := cm.ll[k] + cm.dist[d] + cost[i+k]
					if c < best {
						best = c
						bl, bd = uint16(k), d
					}
				}
				bi++
			}
			// вершина яруса
			c := cm.ll[ltop] + cm.dist[d] + cost[i+ltop]
			if c < best {
				best = c
				bl, bd = uint16(ltop), d
			}
			prevTop = ltop
		}
		cost[i] = best
		bestLen[i], bestDist[i] = bl, bd
	}

	tokens := make([]token, 0, n/3)
	for i := 0; i < n; {
		l := bestLen[i]
		if l == 0 {
			tokens = append(tokens, token{})
			i++
		} else {
			tokens = append(tokens, token{length: l, dist: bestDist[i]})
			i += int(l)
		}
	}
	return tokens
}

// greedy — проход 0 из кэша: жадный разбор длиннейшими матчами
// (нужен только для начальной гистограммы/модели стоимости).
func greedy(data []byte, mc *matchCache) []token {
	n := len(data)
	tokens := make([]token, 0, n/3)
	for i := 0; i < n; {
		a, b := mc.off[i], mc.off[i+1]
		if a < b {
			tp := mc.tier[b-1] // последний ярус — максимальная длина
			tokens = append(tokens, token{length: tp.ltop, dist: tp.dist})
			i += int(tp.ltop)
		} else {
			tokens = append(tokens, token{})
			i++
		}
	}
	return tokens
}

// ---------------------------------------------------------------------------
// Гистограмма и эмит dynamic-блока (BTYPE=10) с RLE длин кодов (16/17/18).
// ---------------------------------------------------------------------------

const (
	symEOB      = 256
	numLitAlph  = 286
	numDistAlph = 30
)

type hist struct {
	litlen [numLitAlph]uint64
	dist   [numDistAlph]uint64
}

func histogram(data []byte, tokens []token) hist {
	var h hist
	pos := 0
	for _, t := range tokens {
		if t.length == 0 {
			h.litlen[data[pos]]++
			pos++
		} else {
			h.litlen[lenToSym[t.length]]++
			h.dist[distToSym[t.dist]]++
			pos += int(t.length)
		}
	}
	h.litlen[symEOB]++ // EOB обязателен
	return h
}

// RLE-кодирование массива длин кодов символами 0..18.
type clOp struct {
	sym      uint8
	extraVal uint16
	extraN   uint8
}

func rleCodeLengths(lens []uint8) []clOp {
	var ops []clOp
	i := 0
	for i < len(lens) {
		v := lens[i]
		run := 1
		for i+run < len(lens) && lens[i+run] == v {
			run++
		}
		rest := run // сколько осталось покрыть
		if v == 0 {
			for rest >= 11 {
				n := rest
				if n > 138 {
					n = 138
				}
				ops = append(ops, clOp{18, uint16(n - 11), 7})
				rest -= n
			}
			for rest >= 3 {
				n := rest
				if n > 10 {
					n = 10
				}
				ops = append(ops, clOp{17, uint16(n - 3), 3})
				rest -= n
			}
			for rest > 0 {
				ops = append(ops, clOp{0, 0, 0})
				rest--
			}
		} else {
			ops = append(ops, clOp{v, 0, 0})
			rest--
			for rest >= 3 {
				n := rest
				if n > 6 {
					n = 6
				}
				ops = append(ops, clOp{16, uint16(n - 3), 2})
				rest -= n
			}
			for rest > 0 {
				ops = append(ops, clOp{v, 0, 0})
				rest--
			}
		}
		i += run // переход через весь обработанный ран
	}
	return ops
}

var clOrder = [19]uint8{16, 17, 18, 0, 8, 7, 9, 6, 10, 5, 11, 4, 12, 3, 13, 2, 14, 1, 15}

// emitStream пишет deflate-поток из токенов; splits — индексы токенов,
// на которых начинаются новые блоки. Каждый блок — dynamic, последний финальный.
func emitStream(data []byte, tokens []token, splits []int) ([]byte, error) {
	w := &bitWriter{buf: make([]byte, 0, len(data)/3)}
	start := 0
	basePos := 0
	bounds := append(append([]int{}, splits...), len(tokens))
	for bi, end := range bounds {
		used, err := emitBlock(w, data, tokens, start, end, basePos, bi == len(bounds)-1)
		if err != nil {
			return nil, err
		}
		start = end
		basePos += used
	}
	return w.finish(), nil
}

// emitBlock: таблицы Хаффмана считаются по токенам [startTok, endTok).
// basePos — позиция в data, с которой начинается сегмент; возвращает число
// обработанных байт data (для инкрементального прохода по сегментам).
func emitBlock(w *bitWriter, data []byte, tokens []token, startTok, endTok, basePos int, final bool) (int, error) {

	// Гистограмма сегмента.
	var h hist
	pos := basePos
	for _, t := range tokens[startTok:endTok] {
		if t.length == 0 {
			h.litlen[data[pos]]++
			pos++
		} else {
			h.litlen[lenToSym[t.length]]++
			h.dist[distToSym[t.dist]]++
			pos += int(t.length)
		}
	}
	h.litlen[symEOB]++

	litlenLen, err := huffmanLengths(h.litlen[:], 15)
	if err != nil {
		return 0, err
	}
	distLen, err := huffmanLengths(h.dist[:], 15)
	if err != nil {
		return 0, err
	}
	allZero := true
	for _, l := range distLen {
		if l > 0 {
			allZero = false
			break
		}
	}
	if allZero {
		distLen[0] = 1 // декодеру нужен хотя бы один код дистанций
	}

	// Числа кодов: HLIT >= 257, HDIST >= 1, HCLEN >= 4.
	numLit := numLitAlph
	for numLit > 257 && litlenLen[numLit-1] == 0 {
		numLit--
	}
	numDist := numDistAlph
	for numDist > 1 && distLen[numDist-1] == 0 {
		numDist--
	}

	// RLE по объединённому массиву длин кодов.
	joined := make([]uint8, 0, numLit+numDist)
	joined = append(joined, litlenLen[:numLit]...)
	joined = append(joined, distLen[:numDist]...)
	ops := rleCodeLengths(joined)

	var clFreq [19]uint64
	for _, op := range ops {
		clFreq[op.sym]++
	}
	clLen, err := huffmanLengths(clFreq[:], 7)
	if err != nil {
		return 0, err
	}
	clCodes := canonicalCodes(clLen)

	numClen := 19
	for numClen > 4 && clLen[clOrder[numClen-1]] == 0 {
		numClen--
	}

	// Заголовок блока.
	if final {
		w.writeBits(1, 1)
	} else {
		w.writeBits(0, 1)
	}
	w.writeBits(2, 2) // BTYPE = 10 (dynamic)
	w.writeBits(uint32(numLit-257), 5)
	w.writeBits(uint32(numDist-1), 5)
	w.writeBits(uint32(numClen-4), 4)
	for i := 0; i < numClen; i++ {
		w.writeBits(uint32(clLen[clOrder[i]]), 3)
	}

	litCodes := canonicalCodes(litlenLen)
	distCodes := canonicalCodes(distLen)

	// Длины кодов (RLE-поток).
	for _, op := range ops {
		w.writeHuff(clCodes[op.sym], clLen[op.sym])
		if op.extraN > 0 {
			w.writeBits(uint32(op.extraVal), uint(op.extraN))
		}
	}

	// Полезная нагрузка.
	pos = basePos
	for _, t := range tokens[startTok:endTok] {
		if t.length == 0 {
			b := data[pos]
			w.writeHuff(litCodes[b], litlenLen[b])
			pos++
		} else {
			s := lenToSym[t.length]
			w.writeHuff(litCodes[s], litlenLen[s])
			e := s - 257
			if lenExtra[e] > 0 {
				w.writeBits(uint32(int(t.length)-lenBase[e]), uint(lenExtra[e]))
			}
			ds := distToSym[t.dist]
			w.writeHuff(distCodes[ds], distLen[ds])
			if distExtra[ds] > 0 {
				w.writeBits(uint32(int(t.dist)-distBase[ds]), uint(distExtra[ds]))
			}
			pos += int(t.length)
		}
	}
	// Символ конца блока (EOB, 256).
	w.writeHuff(litCodes[symEOB], litlenLen[symEOB])
	return pos - basePos, nil
}

// ---------------------------------------------------------------------------
// zlib-обёртка RFC 1950.
// ---------------------------------------------------------------------------

func zlibWrap(deflate []byte, raw []byte) []byte {
	out := make([]byte, 0, len(deflate)+6)
	out = append(out, 0x78, 0x9C)
	out = append(out, deflate...)
	s := adler32(raw)
	out = append(out, byte(s>>24), byte(s>>16), byte(s>>8), byte(s))
	return out
}

// ---------------------------------------------------------------------------
// Главный проход: кэш матчей -> жадный разбор -> итерации squeeze -> выбор
// лучшего по фактическому размеру. Эвристика разбиения на блоки.
// ---------------------------------------------------------------------------

const (
	squeezeIters = 6
	blockTokens  = 4096
)

func zopfliDeflate(raw []byte) ([]byte, error) {
	z := newLZ77(raw)
	mc := z.buildCache() // матчи считаются один раз на все итерации

	toks := greedy(raw, mc) // проход 0: начальная гистограмма/модель стоимости
	h := histogram(raw, toks)

	var best []byte
	for it := 0; it < squeezeIters; it++ {
		ll, err := huffmanLengths(h.litlen[:], 15)
		if err != nil {
			return nil, err
		}
		dd, err := huffmanLengths(h.dist[:], 15)
		if err != nil {
			return nil, err
		}
		cm := newCostModel(ll, dd)
		toks = squeeze(raw, mc, cm)
		out, err := emitStream(raw, toks, nil)
		if err != nil {
			return nil, err
		}
		if best == nil || len(out) < len(best) {
			best = out
		}
		h = histogram(raw, toks)
	}

	// Эвристика разбиения: блоки по ~blockTokens токенов со своими таблицами.
	if len(toks) > 2*blockTokens {
		var splits []int
		for i := blockTokens; i < len(toks); i += blockTokens {
			splits = append(splits, i)
		}
		out, err := emitStream(raw, toks, splits)
		if err != nil {
			return nil, err
		}
		if len(out) < len(best) {
			best = out
		}
	}
	return best, nil
}

// zopfliZlib — сырой поток → zlib-обёртка RFC 1950 (для IDAT PNG).
func zopfliZlib(raw []byte) ([]byte, error) {
	deflate, err := zopfliDeflate(raw)
	if err != nil {
		return nil, err
	}
	return zlibWrap(deflate, raw), nil
}
