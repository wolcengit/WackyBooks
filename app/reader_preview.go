package main

// 本文件定义统一预览结果、格式分派、文件嗅探和文本类预览。
import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

// previewRequest 携带预览分页、工作表选择和请求取消信号。
type previewRequest struct {
	context.Context
	page  int
	sheet int
}

// filePreview 是所有格式共用的前端预览响应结构。
type filePreview struct {
	Type      string        `json:"type"`
	Text      string        `json:"text,omitempty"`
	HTML      string        `json:"html,omitempty"`
	URL       string        `json:"url,omitempty"`
	Table     *previewTable `json:"table,omitempty"`
	Page      int           `json:"page,omitempty"`
	Pages     int           `json:"pages,omitempty"`
	Blocks    []docBlock    `json:"blocks,omitempty"`
	Truncated bool          `json:"truncated,omitempty"`
}

type docBlock struct {
	Text  string `json:"text,omitempty"`
	Image string `json:"image,omitempty"`
}

type previewReader struct {
	kind string
	read func(*os.File, Book, previewRequest) (filePreview, error)
}

var (
	textReader     = previewReader{"text", readTextPreview}
	markdownReader = previewReader{"markdown", readTextPreview}
	imageReader    = previewReader{"image", readImagePreview}
	previewReaders = map[string]previewReader{
		"md": markdownReader, "markdown": markdownReader,
		"log": textReader, "json": textReader, "jsonl": textReader, "yaml": textReader,
		"yml": textReader, "toml": textReader, "ini": textReader, "properties": textReader,
		"csv": {"table", readDelimitedPreview}, "tsv": {"table", readDelimitedPreview},
		"xlsx": {"table", readExcelPreview}, "parquet": {"table", readParquetPreview},
		"docx": {"document", readWordPreview}, "pptx": {"slides", readSlidesPreview},
		"png": imageReader, "jpg": imageReader, "jpeg": imageReader, "gif": imageReader,
		"webp": imageReader, "bmp": imageReader, "avif": imageReader,
	}
	markdown = goldmark.New(goldmark.WithExtensions(extension.GFM))
)

// 限制单次文本、Office 和表格解析的最大读取范围。
const (
	maxTextPreview = 2 << 20
	maxOfficeFile  = 64 << 20
	maxTableFile   = 512 << 20
)

func fileFormat(name string) string {
	return strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
}

func detectImage(data []byte) string {
	switch kind := http.DetectContentType(data); kind {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "image/bmp":
		return kind
	}
	// AVIF is an ISO BMFF image. AVIF may appear in the compatible brands.
	if len(data) >= 16 && string(data[4:8]) == "ftyp" {
		boxLen := uint32(data[0])<<24 | uint32(data[1])<<16 | uint32(data[2])<<8 | uint32(data[3])
		if string(data[8:12]) == "avif" || string(data[8:12]) == "avis" {
			return "image/avif"
		}
		for i := 16; i+4 <= min(len(data), int(boxLen)); i += 4 {
			if string(data[i:i+4]) == "avif" || string(data[i:i+4]) == "avis" {
				return "image/avif"
			}
		}
	}
	return ""
}

func previewHeader(f *os.File) ([]byte, error) {
	var header [512]byte
	n, err := f.ReadAt(header[:], 0)
	if err != nil && err != io.EOF {
		return nil, err
	}
	return header[:n], nil
}

func validText(data []byte) bool {
	for len(data) != 0 {
		r, size := utf8.DecodeRune(data)
		if r == utf8.RuneError && size == 1 || r < ' ' && r != '\n' && r != '\r' && r != '\t' && r != '\f' {
			return false
		}
		data = data[size:]
	}
	return true
}

// checkPreview 使用文件头校验实际内容，避免只凭扩展名解析伪装文件。
func checkPreview(f *os.File, format string) error {
	header, err := previewHeader(f)
	if err != nil {
		return err
	}
	var matches bool
	switch format {
	case "xlsx", "docx", "pptx":
		matches = bytes.HasPrefix(header, []byte("PK\x03\x04"))
	case "parquet":
		matches = bytes.HasPrefix(header, []byte("PAR1"))
	default:
		if previewReaders[format].kind == "image" {
			matches = detectImage(header) != ""
		} else {
			// The sample may end in the middle of a UTF-8 character.
			matches = validText(completeTextPrefix(header))
		}
	}
	if !matches {
		return errors.New("文件内容与支持的格式不符")
	}
	return nil
}

func completeTextPrefix(data []byte) []byte {
	for i := len(data) - 1; i >= 0 && i >= len(data)-utf8.UTFMax; i-- {
		if utf8.RuneStart(data[i]) {
			if !utf8.FullRune(data[i:]) {
				return data[:i]
			}
			break
		}
	}
	return data
}

func servePreview(w http.ResponseWriter, r *http.Request, f *os.File, b Book, size int64) {
	reader, ok := previewReaders[b.Format]
	if !ok {
		fail(w, 404, "此格式没有结构化预览")
		return
	}
	limit := int64(maxTableFile)
	if b.Format == "xlsx" || b.Format == "docx" || b.Format == "pptx" {
		limit = maxOfficeFile
	}
	if size > limit && reader.kind != "image" && reader.kind != "text" && reader.kind != "markdown" {
		fail(w, 422, "文件超过预览大小限制")
		return
	}
	page := 1
	if value := r.URL.Query().Get("page"); value != "" {
		var err error
		page, err = strconv.Atoi(value)
		if err != nil || page < 1 || page > 100000000 {
			fail(w, 400, "预览页码无效")
			return
		}
	}
	if err := checkPreview(f, b.Format); err != nil {
		fail(w, 422, "文件内容与支持的格式不符")
		return
	}
	sheet := 0
	if value := r.URL.Query().Get("sheet"); value != "" {
		var err error
		sheet, err = strconv.Atoi(value)
		if err != nil || sheet < 0 {
			fail(w, 400, "工作表编号无效")
			return
		}
	}
	result, err := reader.read(f, b, previewRequest{r.Context(), page, sheet})
	if err != nil {
		fail(w, 422, "无法预览文件："+err.Error())
		return
	}
	result.Type = reader.kind
	reply(w, result)
}

func readTextPreview(f *os.File, b Book, _ previewRequest) (filePreview, error) {
	data, err := io.ReadAll(io.LimitReader(f, maxTextPreview+1))
	if err != nil {
		return filePreview{}, err
	}
	result := filePreview{Truncated: len(data) > maxTextPreview}
	if result.Truncated {
		data = completeTextPrefix(data[:maxTextPreview])
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if !validText(data) {
		return filePreview{}, errors.New("文本须使用 UTF-8 编码")
	}
	switch b.Format {
	case "md", "markdown":
		var output bytes.Buffer
		if err := markdown.Convert(data, &output); err != nil {
			return filePreview{}, err
		}
		result.HTML = output.String()
	default:
		result.Text = string(data)
		if b.Format == "json" && !result.Truncated {
			var pretty bytes.Buffer
			if json.Indent(&pretty, data, "", "  ") == nil && pretty.Len() <= maxTextPreview {
				result.Text = pretty.String()
			}
		}
	}
	return result, nil
}

func readImagePreview(_ *os.File, b Book, _ previewRequest) (filePreview, error) {
	return filePreview{URL: b.apiPath("/file")}, nil
}
