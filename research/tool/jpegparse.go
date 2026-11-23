package main

import (
	"encoding/binary"
	"fmt"
	"os"
)

// JPEGInfo — структура JPEG: маркеры, режим кодирования, квант-таблицы, субдискретизация.
type JPEGInfo struct {
	Path       string           `json:"path"`
	Progressive bool            `json:"progressive"`
	Width      int              `json:"width"`
	Height     int              `json:"height"`
	Components int              `json:"components"`
	Sampling   string           `json:"sampling"` // например "4:2:0"
	Markers    []MarkerInfo     `json:"markers"`
	QTables    []QTableInfo     `json:"q_tables"`
	EstQuality int              `json:"est_quality"` // приблизительное качество по квант-таблицам
}

type MarkerInfo struct {
	Type string `json:"type"`
	Size int64  `json:"size"`
}

type QTableInfo struct {
	ID       int `json:"id"`
	Precision int `json:"precision"`
	Mean     float64 `json:"mean"`
}

var jpegMarkers = map[byte]string{
	0xC0: "SOF0 baseline", 0xC1: "SOF1 ext-seq", 0xC2: "SOF2 progressive",
	0xC4: "DHT", 0xDB: "DQT", 0xDD: "DRI", 0xE0: "APP0/JFIF", 0xE1: "APP1/EXIF",
	0xE2: "APP2/ICC", 0xEE: "APP14/Adobe", 0xFE: "COM",
}

func parseJPEG(path string) (*JPEGInfo, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) < 4 || raw[0] != 0xFF || raw[1] != 0xD8 {
		return nil, fmt.Errorf("не JPEG: %s", path)
	}
	info := &JPEGInfo{Path: path}
	i := 2
	compSamp := map[byte][2]int{}

	for i+4 <= len(raw) {
		if raw[i] != 0xFF {
			break
		}
		marker := raw[i+1]
		if marker == 0xD8 || marker == 0xD9 || (marker >= 0xD0 && marker <= 0xD7) {
			i += 2
			continue
		}
		if marker == 0xDA { // SOS — дальше энтропийные данные, стоп
			info.Markers = append(info.Markers, MarkerInfo{Type: "SOS", Size: 0})
			break
		}
		segLen := int(binary.BigEndian.Uint16(raw[i+2 : i+4]))
		name := jpegMarkers[marker]
		if name == "" {
			name = fmt.Sprintf("0xFF%02X", marker)
		}
		info.Markers = append(info.Markers, MarkerInfo{Type: name, Size: int64(segLen)})

		body := raw[i+4 : i+2+segLen]
		switch marker {
		case 0xC0, 0xC1, 0xC2:
			info.Progressive = marker == 0xC2
			if len(body) >= 6 {
				info.Height = int(binary.BigEndian.Uint16(body[1:3]))
				info.Width = int(binary.BigEndian.Uint16(body[3:5]))
				info.Components = int(body[5])
				for c := 0; c < info.Components && 6+3*c+2 <= len(body); c++ {
					compSamp[body[6+3*c]] = [2]int{int(body[7+3*c] >> 4), int(body[7+3*c] & 0xF)}
				}
			}
		case 0xDB:
			p := 0
			for p < len(body) {
				pq := body[p] >> 4
				tq := int(body[p] & 0xF)
				p++
				n := 64 * (1 + int(pq))
				if p+n > len(body) {
					break
				}
				sum := 0.0
				for k := 0; k < 64; k++ {
					if pq == 0 {
						sum += float64(body[p+k])
					} else {
						sum += float64(binary.BigEndian.Uint16(body[p+2*k : p+2*k+2]))
					}
				}
				info.QTables = append(info.QTables, QTableInfo{ID: tq, Precision: int(pq), Mean: sum / 64})
				p += n
			}
		}
		i += 2 + segLen
	}

	// Субдискретизация по компонентам (упрощённо: по Y и Cb).
	if len(compSamp) >= 3 {
		var y, cb [2]int
		first := true
		for _, v := range compSamp {
			if first {
				y, first = v, false
			} else if v != y {
				cb = v
			}
		}
		switch {
		case y == [2]int{2, 2} && cb[0] <= 1:
			info.Sampling = "4:2:0"
		case y == [2]int{2, 1}:
			info.Sampling = "4:2:2"
		case y == [2]int{1, 1}:
			info.Sampling = "4:4:4"
		default:
			info.Sampling = fmt.Sprintf("%dx%d", y[0], y[1])
		}
	}

	info.EstQuality = estimateJPEGQuality(info.QTables)
	return info, nil
}

// estimateJPEGQuality — грубая оценка качества по средним значениям квант-таблиц
// относительно стандартных таблиц из Annex K спецификации JPEG.
func estimateJPEGQuality(tables []QTableInfo) int {
	if len(tables) == 0 {
		return 0
	}
	// Среднее стандартной таблицы яркости (Annex K) = 32.203125.
	const stdMean = 32.203125
	mean := 0.0
	for _, t := range tables {
		mean += t.Mean
	}
	mean /= float64(len(tables))
	if mean <= 0 {
		return 0
	}
	scale := mean / stdMean // ~ (200 - 2q)/100 при q>=50, обратная задача
	q := 100 - int(scale*50)
	if q < 1 {
		q = 1
	}
	if q > 100 {
		q = 100
	}
	return q
}
