package compress

import (
	"math/rand"
	"testing"
)

// bruteNearest — эталонный перебор с тем же tie-break (меньший индекс при равных).
func bruteNearest(pal [][4]uint8, p [4]uint8) uint8 {
	best, bestD := uint8(0), 1<<30
	for i, q := range pal {
		if d := sqDist(p, q); d < bestD {
			best, bestD = uint8(i), d
		}
	}
	return best
}

// TestKDMatchesBrute — k-d дерево обязано давать ровно тот же индекс,
// что и перебор, включая дубликаты цветов в палитре (tie-break по индексу).
func TestKDMatchesBrute(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for trial := 0; trial < 100; trial++ {
		n := 1 + rng.Intn(256)
		pal := make([][4]uint8, n)
		for i := range pal {
			// Маленький диапазон каналов плодит дубликаты и равные расстояния.
			pal[i] = [4]uint8{
				uint8(rng.Intn(8) * 32), uint8(rng.Intn(8) * 32),
				uint8(rng.Intn(8) * 32), uint8(rng.Intn(4) * 64),
			}
		}
		tree := buildKD(pal)
		for q := 0; q < 2000; q++ {
			p := [4]uint8{uint8(rng.Intn(256)), uint8(rng.Intn(256)),
				uint8(rng.Intn(256)), uint8(rng.Intn(256))}
			want := bruteNearest(pal, p)
			got, _ := tree.query(p, 0, 1<<30)
			if got != want {
				t.Fatalf("trial %d: p=%v: k-d дал %d, перебор %d", trial, p, got, want)
			}
		}
	}
}
