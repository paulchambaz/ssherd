package static

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed css/styles.css js/htmx.min.js js/htmx-ws.min.js js/main.js svg/up.svg svg/down.svg svg/left.svg svg/right.svg svg/moon.svg svg/sun.svg svg/cross.svg svg/hide.svg svg/minus.svg svg/pencil.svg svg/plus.svg svg/show.svg svg/users.svg svg/copy.svg
var content embed.FS

func FileSystem() http.FileSystem {
	fsys, err := fs.Sub(content, ".")
	if err != nil {
		panic(err)
	}
	return http.FS(fsys)
}

