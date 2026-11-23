package main

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// PNGInfo — структура PNG-файла: чанки, формат пикселей, фильтры строк.
type PNGInfo struct {
	Path          string      `json:"path"`
	Width         int         `json:"width"`
	Height        int         `json:"height"`
	BitDepth      int         `json:"bit_depth"`
	ColorType     int         `json:"color_type"`
	ColorTypeName string      `json:"color_type_name"`
	Interlace     int         `json:"interlace"`
	Chunks        []ChunkInfo `json:"chunks"`
	PaletteUsed   int         `json:"palette_used"`   // реально использованных цветов палитры
	PaletteMax    int         `json:"palette_max"`    // записей в PLTE
	HasTRNS       bool        `json:"has_trns"`
	TextChunks    []string    `json:"text_chunks"`
	IDATComp      int64       `json:"idat_compressed"`
	IDATRaw       int64       `json:"idat_raw"`
	FilterHist    [5]int64    `json:"filter_hist"` // None, Sub, Up, Average, Paeth
}

type ChunkInfo struct {
	Type string `json:"type"`
	Size int64  `json:"size"`
}

var colorTypeNames = map[int]string{
	0: "grayscale",
	2: "truecolor RGB",
	3: "indexed (palette)",
	4: "grayscale+alpha",
	6: "truecolor+alpha",
}

func parsePNG(path string) (*PNGInfo, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	info := &PNGInfo{Path: path}
	r := bytes.NewReader(raw)

	var sig [8]byte
	if _, err := io.ReadFull(r, sig[:]); err != nil {
		return nil, fmt.Errorf("не PNG: %w", err)
	}
	plteEntries := 0
	var idatBuf bytes.Buffer

	for {
		var lenBuf [4]byte
		if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
			break // конец файла
		}
		length := int64(binary.BigEndian.Uint32(lenBuf[:]))
		var typBuf [4]byte
		if _, err := io.ReadFull(r, typBuf[:]); err != nil {
			return nil, err
		}
		typ := string(typBuf[:])
		data := make([]byte, length)
		if _, err := io.ReadFull(r, data); err != nil {
			return nil, err
		}
		var crc [4]byte
		if _, err := io.ReadFull(r, crc[:]); err != nil {
			return nil, err
		}

		info.Chunks = append(info.Chunks, ChunkInfo{Type: typ, Size: length})

		switch typ {
		case "IHDR":
			info.Width = int(binary.BigEndian.Uint32(data[0:4]))
			info.Height = int(binary.BigEndian.Uint32(data[4:8]))
			info.BitDepth = int(data[8])
			info.ColorType = int(data[9])
			info.ColorTypeName = colorTypeNames[info.ColorType]
			info.Interlace = int(data[12])
		case "PLTE":
			plteEntries = len(data) / 3
		case "tRNS":
			info.HasTRNS = true
		case "tEXt", "zTXt", "iTXt":
			nul := bytes.IndexByte(data, 0)
			key := string(data)
			if nul >= 0 {
				key = string(data[:nul])
			}
			info.TextChunks = append(info.TextChunks, key)
		case "IDAT":
			info.IDATComp += length
			idatBuf.Write(data)
		}
	}
	info.PaletteMax = plteEntries

	// Распаковываем IDAT: сырой объём + гистограмма фильтров строк.
	if idatBuf.Len() > 0 {
		zr, err := zlib.NewReader(&idatBuf)
		if err == nil {
			rawScan, err := io.ReadAll(zr)
			zr.Close()
			if err == nil {
				info.IDATRaw = int64(len(rawScan))
				info.FilterHist = filterHistogram(rawScan, info.Width, info.Height, info.ColorType, info.BitDepth)
			}
		}
	}
	return info, nil
}

// filterHistogram — какие фильтры строк выбрал энкодер (None/Sub/Up/Average/Paeth).
func filterHistogram(data []byte, w, h, colorType, bitDepth int) [5]int64 {
	var hist [5]int64
	channels := map[int]int{0: 1, 2: 3, 3: 1, 4: 2, 6: 4}[colorType]
	bpp := channels * bitDepth / 8
	if bpp == 0 {
		return hist
	}
	stride := 1 + w*bpp
	if h == 0 || int64(len(data)) < int64(stride)*int64(h) {
		return hist
	}
	for y := 0; y < h; y++ {
		f := data[y*stride]
		if f <= 4 {
			hist[f]++
		}
	}
	return hist
}
