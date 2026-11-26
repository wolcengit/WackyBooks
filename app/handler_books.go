package main

// 本文件管理书库配置、目录扫描、书籍元数据以及书籍文件服务。
import (
	"archive/zip"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Metadata 是从书籍文件和同名 JSON 文件读取的通用书目字段。
type Metadata struct {
	Title                                   string
	Authors                                 []string
	Publisher                               string
	Subjects                                []string
	Description, Language, Identifier, Date string
}

// Book 是扫描后可展示和阅读的书籍记录，root 等字段仅供服务端使用。
type Book struct {
	Metadata
	ID                     int64
	Library                int64
	Path, Format           string
	Added                  time.Time
	Pages, FavoriteCount   int
	root, cover, epubCover string
	chapters               []string
}

// library 新建、更新或删除书库配置，并在配置变化后刷新书目索引。
func (s *Server) library(w http.ResponseWriter, r *http.Request) {
	var lib Library
	if !decode(w, r, &lib) {
		return
	}
	if r.Method == "POST" {
		lib.Name = strings.TrimSpace(lib.Name)
		lib.Directory = strings.TrimSpace(lib.Directory)
		if lib.Name == "" || !filepath.IsAbs(lib.Directory) {
			fail(w, 400, "请输入书库名称和绝对目录")
			return
		}
		dir, err := filepath.EvalSymlinks(lib.Directory)
		if err != nil {
			fail(w, 400, "目录不存在")
			return
		}
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			fail(w, 400, "目录不可用")
			return
		}
		f, err := os.Open(dir)
		if err != nil {
			fail(w, 400, "目录不可读")
			return
		}
		f.Close()
		lib.Directory = dir
	}
	if s.update(w, func(next *State) error {
		for i, item := range next.Libraries {
			if item.ID != lib.ID {
				continue
			}
			if r.Method == "DELETE" || item.Directory != lib.Directory {
				for id, user := range next.Users {
					user.LibraryIDs = slices.DeleteFunc(user.LibraryIDs, func(id int64) bool { return id == item.ID })
					next.Users[id] = user
				}
			}
			if r.Method == "DELETE" {
				next.Libraries = append(next.Libraries[:i], next.Libraries[i+1:]...)
			} else {
				if item.Directory != lib.Directory {
					lib.ID = 0
				}
				lib.key = item.key
				next.Libraries[i] = lib
			}
			return nil
		}
		if lib.ID != 0 || r.Method == "DELETE" {
			return errors.New("书库不存在")
		}
		next.Libraries = append(next.Libraries, lib)
		return nil
	}) {
		if err := s.scan(); err != nil {
			s.books = nil
			storageError(w, err)
			return
		}
		reply(w, true)
	}
}

// rescanLibrary 处理管理员发起的单书库重扫请求。
func (s *Server) rescanLibrary(w http.ResponseWriter, r *http.Request) {
	var in struct{ ID int64 }
	if !decode(w, r, &in) {
		return
	}
	if in.ID <= 0 {
		fail(w, http.StatusBadRequest, "书库编号无效")
		return
	}
	if err := s.scanLibrary(in.ID); err != nil {
		switch {
		case errors.Is(err, errLibraryNotFound):
			fail(w, http.StatusNotFound, err.Error())
		case errors.Is(err, errLibraryUnavailable):
			fail(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, errScanNotReady):
			fail(w, http.StatusConflict, "书库索引尚未初始化，请刷新页面后重试")
		default:
			storageError(w, err)
		}
		return
	}
	reply(w, true)
}

type directoryOption struct {
	Name string
	Path string
}

type directoryListing struct {
	Path        string
	Parent      string
	Directories []directoryOption
}

// browseDirectories 为管理员列出指定目录中的子目录，供书库路径选择器使用。
func browseDirectories(w http.ResponseWriter, r *http.Request) {
	directory := strings.TrimSpace(r.URL.Query().Get("path"))
	if directory == "" {
		workingDirectory, err := os.Getwd()
		if err != nil {
			fail(w, http.StatusInternalServerError, "无法读取当前目录")
			return
		}
		directory = workingDirectory
	}
	if !filepath.IsAbs(directory) {
		fail(w, http.StatusBadRequest, "目录路径必须是绝对路径")
		return
	}

	resolved, err := filepath.EvalSymlinks(filepath.Clean(directory))
	if err != nil {
		fail(w, http.StatusBadRequest, "目录不存在或不可访问")
		return
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		fail(w, http.StatusBadRequest, "请选择有效目录")
		return
	}
	entries, err := os.ReadDir(resolved)
	if err != nil {
		fail(w, http.StatusForbidden, "无法读取该目录")
		return
	}

	listing := directoryListing{Path: resolved, Directories: make([]directoryOption, 0)}
	parent := filepath.Dir(resolved)
	if parent != resolved {
		listing.Parent = parent
	}
	for _, entry := range entries {
		isDirectory := entry.IsDir()
		if !isDirectory && entry.Type()&os.ModeSymlink != 0 {
			childInfo, err := os.Stat(filepath.Join(resolved, entry.Name()))
			isDirectory = err == nil && childInfo.IsDir()
		}
		if !isDirectory {
			continue
		}
		childPath := filepath.Join(resolved, entry.Name())
		if childPath, err = filepath.EvalSymlinks(childPath); err != nil {
			continue
		}
		listing.Directories = append(listing.Directories, directoryOption{Name: entry.Name(), Path: childPath})
	}
	reply(w, listing)
}

var (
	errLibraryNotFound    = errors.New("书库不存在")
	errLibraryUnavailable = errors.New("书库目录不可用")
	errScanNotReady       = errors.New("书库索引尚未初始化")
)

// scan 遍历全部已配置书库，提取元数据并重建目录与书目索引。
func (s *Server) scan() error {
	return s.scanScope(0)
}

// scanLibrary 只重扫指定书库，并保留其他书库的扫描结果。
func (s *Server) scanLibrary(libraryID int64) error {
	var library *Library
	for i := range s.state.Libraries {
		if s.state.Libraries[i].ID == libraryID {
			library = &s.state.Libraries[i]
			break
		}
	}
	if library == nil {
		return errLibraryNotFound
	}
	if s.books == nil {
		return errScanNotReady
	}
	info, err := os.Stat(library.Directory)
	if err != nil || !info.IsDir() {
		return errLibraryUnavailable
	}
	root, err := os.Open(library.Directory)
	if err != nil {
		return errLibraryUnavailable
	}
	root.Close()
	return s.scanScope(libraryID)
}

func (s *Server) scanScope(libraryID int64) error {
	books := map[string]Book{}
	sidecars := map[string]bool{}
	directories := map[int64][]string{}
	for _, lib := range s.state.Libraries {
		if libraryID != 0 && lib.ID != libraryID {
			continue
		}
		filepath.WalkDir(lib.Directory, func(file string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if rel, err := filepath.Rel(lib.Directory, file); err == nil && rel != "." {
					directories[lib.ID] = append(directories[lib.ID], filepath.ToSlash(rel))
				}
				return nil
			}
			if !d.Type().IsRegular() {
				return nil
			}
			format := fileFormat(file)
			reader, known := previewReaders[format]
			if !known && format != "pdf" && format != "epub" && format != "txt" {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			rel, err := filepath.Rel(lib.Directory, file)
			if err != nil {
				return nil
			}
			base := strings.TrimSuffix(rel, filepath.Ext(rel))
			b := Book{Library: lib.ID, Path: filepath.ToSlash(rel), Format: format, Added: info.ModTime(), root: lib.Directory}
			b.Title = filepath.Base(base)
			if format == "epub" {
				b.loadEPUB()
			}
			if format == "pdf" {
				b.loadPDF()
			}
			if root, err := os.OpenRoot(lib.Directory); err == nil {
				if format != "json" {
					if f, err := root.Open(base + ".json"); err == nil {
						var override Metadata
						if json.NewDecoder(io.LimitReader(f, 65536)).Decode(&override) == nil {
							if override.Title != "" || override.Authors != nil || override.Publisher != "" || override.Subjects != nil ||
								override.Description != "" || override.Language != "" || override.Identifier != "" || override.Date != "" {
								sidecars[bookKey(lib.key, base+".json")] = true
							}
							if override.Title != "" {
								b.Title = override.Title
							}
							if override.Authors != nil {
								b.Authors = override.Authors
							}
							if override.Publisher != "" {
								b.Publisher = override.Publisher
							}
							if override.Subjects != nil {
								b.Subjects = override.Subjects
							}
							if override.Description != "" {
								b.Description = override.Description
							}
							if override.Language != "" {
								b.Language = override.Language
							}
							if override.Identifier != "" {
								b.Identifier = override.Identifier
							}
							if override.Date != "" {
								b.Date = override.Date
							}
						}
						f.Close()
					}
				}
				if reader.kind != "image" {
					for _, ext := range []string{".jpg", ".jpeg", ".png", ".webp"} {
						if info, err := root.Stat(base + ext); err == nil && info.Mode().IsRegular() {
							b.cover = base + ext
							sidecars[bookKey(lib.key, b.cover)] = true
							break
						}
					}
				}
				root.Close()
			}
			books[bookKey(lib.key, rel)] = b
			return nil
		})
	}
	for id := range sidecars {
		delete(books, id)
	}
	if err := s.indexBookSet(books, libraryID); err != nil {
		return err
	}
	if libraryID == 0 {
		s.books = make(map[int64]Book, len(books))
		for _, book := range books {
			s.books[book.ID] = book
		}
		s.directories = directories
		return nil
	}
	nextBooks := make(map[int64]Book, len(s.books)+len(books))
	for id, book := range s.books {
		if book.Library != libraryID {
			nextBooks[id] = book
		}
	}
	for _, book := range books {
		nextBooks[book.ID] = book
	}
	s.books = nextBooks
	if s.directories == nil {
		s.directories = make(map[int64][]string)
	}
	s.directories[libraryID] = directories[libraryID]
	return nil
}

func (b Book) open() (*os.File, error) {
	root, err := os.OpenRoot(b.root)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.Open(filepath.FromSlash(b.Path))
}

func (b *Book) loadPDF() {
	f, err := b.open()
	if err != nil {
		return
	}
	defer f.Close()
	cmd := exec.Command("pdfinfo", "/proc/self/fd/3")
	cmd.ExtraFiles = []*os.File{f}
	out, err := cmd.Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		key, value, _ := strings.Cut(line, ":")
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		switch key {
		case "Title":
			b.Title = value
		case "Author":
			b.Authors = []string{value}
		case "Publisher":
			b.Publisher = value
		case "Subject":
			b.Description = value
		case "Keywords":
			b.Subjects = []string{value}
		case "CreationDate":
			b.Date = value
		case "Pages":
			b.Pages, _ = strconv.Atoi(value)
		}
	}
}

func archive(f *os.File) (*zip.Reader, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return zip.NewReader(f, info.Size())
}

func zipData(z *zip.Reader, name string, limit int64) ([]byte, error) {
	for _, entry := range z.File {
		if entry.Name != name {
			continue
		}
		if entry.UncompressedSize64 > uint64(limit) {
			return nil, errors.New("资源过大")
		}
		f, err := entry.Open()
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return io.ReadAll(io.LimitReader(f, limit))
	}
	return nil, os.ErrNotExist
}

func (b *Book) loadEPUB() {
	f, err := b.open()
	if err != nil {
		return
	}
	defer f.Close()
	z, err := archive(f)
	if err != nil {
		return
	}
	container, err := zipData(z, "META-INF/container.xml", 1<<20)
	if err != nil {
		return
	}
	// A container can list multiple renditions. Use its first package.
	var root struct {
		Files []struct {
			Path string `xml:"full-path,attr"`
		} `xml:"rootfiles>rootfile"`
	}
	if xml.Unmarshal(container, &root) != nil || len(root.Files) == 0 {
		return
	}
	packagePath := root.Files[0].Path
	data, err := zipData(z, packagePath, 2<<20)
	if err != nil {
		return
	}
	var pkg struct {
		Metadata struct {
			Title       string   `xml:"title"`
			Authors     []string `xml:"creator"`
			Publisher   string   `xml:"publisher"`
			Subjects    []string `xml:"subject"`
			Description string   `xml:"description"`
			Language    string   `xml:"language"`
			Identifier  string   `xml:"identifier"`
			Date        string   `xml:"date"`
			Meta        []struct {
				Name    string `xml:"name,attr"`
				Content string `xml:"content,attr"`
			} `xml:"meta"`
		} `xml:"metadata"`
		Items []struct {
			ID         string `xml:"id,attr"`
			Href       string `xml:"href,attr"`
			Properties string `xml:"properties,attr"`
		} `xml:"manifest>item"`
		Spine []struct {
			ID string `xml:"idref,attr"`
		} `xml:"spine>itemref"`
	}
	if xml.Unmarshal(data, &pkg) != nil {
		return
	}
	if pkg.Metadata.Title != "" {
		b.Title = pkg.Metadata.Title
	}
	b.Authors, b.Publisher = pkg.Metadata.Authors, pkg.Metadata.Publisher
	b.Subjects, b.Description = pkg.Metadata.Subjects, pkg.Metadata.Description
	b.Language, b.Identifier, b.Date = pkg.Metadata.Language, pkg.Metadata.Identifier, pkg.Metadata.Date
	coverID := ""
	for _, meta := range pkg.Metadata.Meta {
		if meta.Name == "cover" {
			coverID = meta.Content
		}
	}
	items := map[string]string{}
	for _, item := range pkg.Items {
		u, err := url.Parse(item.Href)
		if err != nil || u.IsAbs() || u.Host != "" {
			continue
		}
		name := path.Join(path.Dir(packagePath), u.Path)
		items[item.ID] = name
		if strings.Contains(" "+item.Properties+" ", " cover-image ") || (coverID != "" && item.ID == coverID) {
			b.epubCover = name
		}
	}
	for _, ref := range pkg.Spine {
		if item := items[ref.ID]; item != "" {
			b.chapters = append(b.chapters, item)
		}
	}
}

// serveBook 按操作提供原文件、封面、页面、章节或预览资源。
func serveBook(w http.ResponseWriter, r *http.Request, b Book) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	action := r.PathValue("action")
	if action == "cover" && b.cover != "" {
		root, err := os.OpenRoot(b.root)
		if err == nil {
			defer root.Close()
			if f, err := root.Open(b.cover); err == nil {
				defer f.Close()
				if info, err := f.Stat(); err == nil && info.Mode().IsRegular() {
					if header, err := previewHeader(f); err == nil && detectImage(header) != "" {
						w.Header().Set("Content-Type", detectImage(header))
						http.ServeContent(w, r, b.cover, info.ModTime(), f)
						return
					}
				}
			}
		}
	}
	f, err := b.open()
	if err != nil {
		fail(w, 404, "无法打开书籍")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		fail(w, 404, "无法打开书籍")
		return
	}
	switch action {
	case "preview":
		servePreview(w, r, f, b, info.Size())
	case "preview-image":
		name := r.URL.Query().Get("name")
		if b.Format == "docx" && info.Size() <= maxOfficeFile && strings.HasPrefix(name, "word/media/") && path.Clean(name) == name {
			if z, err := archive(f); err == nil {
				if data, err := zipData(z, name, 8<<20); err == nil {
					if kind := detectImage(data); kind != "" {
						w.Header().Set("Content-Type", kind)
						w.Write(data)
						return
					}
				}
			}
		}
		fail(w, 404, "图片不存在或无法预览")
	case "download":
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filepath.Base(b.Path)}))
		http.ServeContent(w, r, filepath.Base(b.Path), info.ModTime(), f)
	case "file":
		switch b.Format {
		case "txt":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		case "pdf":
			w.Header().Set("Content-Type", "application/pdf")
		case "epub":
			w.Header().Set("Content-Type", "application/epub+zip")
			w.Header().Set("Content-Disposition", "attachment")
		default:
			if previewReaders[b.Format].kind != "image" || !serveImage(w, r, f, b, info) {
				fail(w, 404, "文件无法内嵌显示")
			}
			return
		}
		http.ServeContent(w, r, filepath.Base(b.Path), info.ModTime(), f)
	case "cover":
		if previewReaders[b.Format].kind == "image" && serveImage(w, r, f, b, info) {
			return
		}
		if b.Format == "pdf" {
			cmd := exec.CommandContext(r.Context(), "pdftoppm", "-f", "1", "-l", "1", "-singlefile", "-scale-to", "420", "-png", "/proc/self/fd/3")
			cmd.ExtraFiles = []*os.File{f}
			if data, err := cmd.Output(); err == nil {
				w.Header().Set("Content-Type", "image/png")
				w.Write(data)
				return
			}
		}
		if b.epubCover != "" {
			if z, err := archive(f); err == nil {
				if data, err := zipData(z, b.epubCover, 8<<20); err == nil {
					kind := http.DetectContentType(data)
					if kind == "image/png" || kind == "image/jpeg" || kind == "image/gif" || kind == "image/webp" {
						w.Header().Set("Content-Type", kind)
						w.Write(data)
						return
					}
				}
			}
		}
		fail(w, 404, "书籍没有封面")
	case "page":
		n, err := strconv.Atoi(r.URL.Query().Get("n"))
		if b.Format != "pdf" || err != nil || n < 1 || n > b.Pages {
			fail(w, 404, "页面不存在")
			return
		}
		page := strconv.Itoa(n)
		cmd := exec.CommandContext(r.Context(), "pdftoppm", "-f", page, "-l", page, "-singlefile", "-scale-to", "1600", "-png", "/proc/self/fd/3")
		cmd.ExtraFiles = []*os.File{f}
		data, err := cmd.Output()
		if err != nil || len(data) == 0 {
			fail(w, 422, "无法加载 PDF 页面")
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Write(data)
	case "chapters":
		reply(w, len(b.chapters))
	case "chapter":
		n, err := strconv.Atoi(r.URL.Query().Get("n"))
		if err != nil || n < 0 || n >= len(b.chapters) {
			fail(w, 404, "章节不存在")
			return
		}
		target := url.URL{Path: b.apiPath("/assets/") + b.chapters[n]}
		http.Redirect(w, r, target.String(), http.StatusFound)
	case "assets":
		if b.Format != "epub" {
			fail(w, 404, "资源不存在")
			return
		}
		z, err := archive(f)
		if err != nil {
			fail(w, 404, "无法打开 EPUB")
			return
		}
		name := r.PathValue("resource")
		data, err := zipData(z, name, 16<<20)
		if err != nil {
			fail(w, 404, "资源不存在")
			return
		}
		kind := mime.TypeByExtension(strings.ToLower(path.Ext(name)))
		if kind == "" {
			kind = "application/octet-stream"
		}
		w.Header().Set("Content-Type", kind)
		w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; font-src 'self' data:; frame-ancestors 'self'; base-uri 'none'")
		w.Write(data)
	default:
		fail(w, 404, "资源不存在")
	}
}

func serveImage(w http.ResponseWriter, r *http.Request, f *os.File, b Book, info os.FileInfo) bool {
	header, err := previewHeader(f)
	if err != nil {
		return false
	}
	kind := detectImage(header)
	if kind == "" {
		return false
	}
	w.Header().Set("Content-Type", kind)
	http.ServeContent(w, r, filepath.Base(b.Path), info.ModTime(), f)
	return true
}

func (b Book) apiPath(suffix string) string {
	return "/api/book/" + strconv.FormatInt(b.ID, 10) + suffix
}
