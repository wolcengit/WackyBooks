package main

// 本文件提供密码派生函数，并将 Vue 前端静态资源嵌入可执行文件。
import (
	"crypto/pbkdf2"
	"crypto/sha256"
	"embed"
	"fmt"
	"hash/fnv"
	"io/fs"
	"net/http"
	"strings"
)

func pbkdf2Key(password string, salt []byte) ([]byte, error) {
	return pbkdf2.Key(sha256.New, password, salt, 600000, 32)
}

//go:embed web
var assets embed.FS

func frontend() http.Handler {
	files, _ := fs.Sub(assets, "web")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			hash := fnv.New64a()
			for _, path := range []string{"index.html", "style.css", "app.js"} {
				data, err := fs.ReadFile(files, path)
				if err != nil {
					http.Error(w, "前端资源不可用", http.StatusInternalServerError)
					return
				}
				_, _ = hash.Write(data)
			}
			version := fmt.Sprintf("%x", hash.Sum64())
			index, err := fs.ReadFile(files, "index.html")
			if err != nil {
				http.Error(w, "前端资源不可用", http.StatusInternalServerError)
				return
			}
			index = []byte(strings.ReplaceAll(string(index), "__ASSET_VERSION__", version))
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(index)
			return
		}
		http.FileServer(http.FS(files)).ServeHTTP(w, r)
	})
}
