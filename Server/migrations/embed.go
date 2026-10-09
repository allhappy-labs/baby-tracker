package migrations

import (
	"embed"
	"io/fs"
)

//go:embed *.sql
var embedded embed.FS

var Files fs.FS = embedded
