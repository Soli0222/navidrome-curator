// Package web は画面のファイルをバイナリに同梱する。
package web

import (
	"embed"
	"io/fs"
)

//go:embed static
var files embed.FS

// FS は画面のファイルを返す。
func FS() fs.FS {
	sub, err := fs.Sub(files, "static")
	if err != nil {
		panic(err)
	}
	return sub
}
