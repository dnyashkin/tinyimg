// Package web отдаёт статику веб-интерфейса, встроенную в бинарь.
package web

import (
	"embed"
	"io/fs"
)

//go:embed static
var static embed.FS

// FS — файловая система статики (index.html, app.js, styles.css).
func FS() fs.FS {
	sub, err := fs.Sub(static, "static")
	if err != nil {
		panic(err)
	}
	return sub
}
