package compress

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"hash/crc32"
	"io"
)

// repackPNG — лосслесс-переупаковка IDAT через zopfli-энкодер (zopfliZlib):
// inflate → zopfli-deflate, пиксели не меняются вообще. Выигрыш −6…−7%
// против flate BestCompression (на дизеренных палитровых данных).
// При любой ошибке разбора возвращает исходные байты без изменений.
func repackPNG(src []byte) []byte {
	const sigLen = 8
	if len(src) < sigLen+25 || !bytes.Equal(src[:sigLen], []byte{137, 80, 78, 71, 13, 10, 26, 10}) {
		return src
	}
	var idat []byte
	var chunks []pngChunk // все чанки в порядке следования (IDAT соберём)
	pos := sigLen
	for pos+12 <= len(src) {
		n := binary.BigEndian.Uint32(src[pos:])
		typ := string(src[pos+4 : pos+8])
		if pos+12+int(n) > len(src) {
			return src // битый файл — не трогаем
		}
		data := src[pos+8 : pos+8+int(n)]
		if typ == "IDAT" {
			idat = append(idat, data...)
		} else {
			c := pngChunk{typ: typ, data: append([]byte(nil), data...)}
			if typ == "IEND" {
				chunks = append(chunks, c)
				break
			}
			chunks = append(chunks, c)
		}
		pos += 12 + int(n)
	}
	if len(idat) == 0 {
		return src
	}
	zr, err := zlib.NewReader(bytes.NewReader(idat))
	if err != nil {
		return src
	}
	raw, err := io.ReadAll(zr)
	zr.Close()
	if err != nil {
		return src
	}
	fresh, err := zopfliZlib(raw)
	if err != nil {
		return src // внутренняя ошибка энкодера — не трогаем исходник
	}
	if len(fresh) >= len(idat) {
		return src // zopfli не помог (мелкий/уже оптимальный поток) — не раздуваем
	}

	// Собираем файл обратно: сигнатура + чанки, IDAT — одним свежим.
	var out bytes.Buffer
	out.Write(src[:sigLen])
	for _, c := range chunks {
		if c.typ == "IEND" {
			writePNGChunk(&out, "IDAT", fresh)
		}
		writePNGChunk(&out, c.typ, c.data)
	}
	return out.Bytes()
}

type pngChunk struct {
	typ  string
	data []byte
}

func writePNGChunk(buf *bytes.Buffer, typ string, data []byte) {
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[:4], uint32(len(data)))
	copy(hdr[4:], typ)
	buf.Write(hdr[:])
	buf.Write(data)
	crc := crc32.NewIEEE()
	crc.Write(hdr[4:])
	crc.Write(data)
	var c [4]byte
	binary.BigEndian.PutUint32(c[:], crc.Sum32())
	buf.Write(c[:])
}
