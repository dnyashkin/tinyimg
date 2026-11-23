//go:build !cgo

package compress

import "errors"

// Заглушка для сборок без cgo: webp-кодек требует libwebp (cgo).
func compressWebP(src, dst string, opts Options, stage *stageLogger) (int64, int64, string, error) {
	return 0, 0, "", errors.New("webp поддерживается только в сборке с cgo (libwebp)")
}

// WebPSupported — доступен ли webp-кодек в этой сборке.
func WebPSupported() bool { return false }
