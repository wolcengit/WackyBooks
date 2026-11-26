package main

// 本文件从 DOCX 和 PPTX 压缩包中提取正文、幻灯片文字和受限内嵌图片。
import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"net/url"
	"os"
	"path"
	"strings"
)

const maxOfficeXML = 8 << 20

type officeRelationship struct {
	Type       string `xml:"Type,attr"`
	ID         string `xml:"Id,attr"`
	Target     string `xml:"Target,attr"`
	TargetMode string `xml:"TargetMode,attr"`
}

// officeRelationships 只接受包内的指定资源，拒绝外链和越界路径。
func officeRelationships(z *zip.Reader, part, kind, directory string) (map[string]string, error) {
	relationshipsPart := path.Join(path.Dir(part), "_rels", path.Base(part)+".rels")
	data, err := zipData(z, relationshipsPart, 1<<20)
	if err != nil {
		return nil, err
	}
	var relationships struct {
		Items []officeRelationship `xml:"Relationship"`
	}
	if err := xml.Unmarshal(data, &relationships); err != nil {
		return nil, err
	}
	targets := make(map[string]string)
	for _, rel := range relationships.Items {
		if !strings.HasSuffix(rel.Type, "/"+kind) || strings.EqualFold(rel.TargetMode, "External") {
			continue
		}
		u, err := url.Parse(rel.Target)
		if err != nil || u.IsAbs() || u.Host != "" || u.Path == "" {
			continue
		}
		name := path.Clean(strings.TrimPrefix(u.Path, "/"))
		if !strings.HasPrefix(u.Path, "/") {
			name = path.Join(path.Dir(part), u.Path)
		}
		if strings.HasPrefix(name, directory+"/") {
			targets[rel.ID] = name
		}
	}
	return targets, nil
}

func relationshipID(attributes []xml.Attr, name string) string {
	for _, attr := range attributes {
		if attr.Name.Local == name && strings.HasSuffix(attr.Name.Space, "/relationships") {
			return attr.Value
		}
	}
	return ""
}

func readWordPreview(f *os.File, b Book, req previewRequest) (filePreview, error) {
	z, err := archive(f)
	if err != nil {
		return filePreview{}, errors.New("DOCX 文件包无效")
	}
	const part = "word/document.xml"
	data, err := zipData(z, part, maxOfficeXML)
	if err != nil {
		return filePreview{}, errors.New("无法读取 DOCX 文档内容")
	}
	images, err := officeRelationships(z, part, "image", "word/media")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return filePreview{}, errors.New("DOCX 图片关系无效")
	}
	return officeBlocks(data, req, func(element xml.StartElement) string {
		name := images[relationshipID(element.Attr, "embed")]
		if element.Name.Local != "blip" || name == "" {
			return ""
		}
		return b.apiPath("/preview-image?") + url.Values{"name": {name}}.Encode()
	})
}

func readSlidesPreview(f *os.File, _ Book, req previewRequest) (filePreview, error) {
	z, err := archive(f)
	if err != nil {
		return filePreview{}, errors.New("PPTX 文件包无效")
	}
	const part = "ppt/presentation.xml"
	data, err := zipData(z, part, 2<<20)
	if err != nil {
		return filePreview{}, errors.New("无法读取 PPTX 演示文稿")
	}
	var presentation struct {
		Slides []struct {
			Attributes []xml.Attr `xml:",any,attr"`
		} `xml:"sldIdLst>sldId"`
	}
	if err := xml.Unmarshal(data, &presentation); err != nil {
		return filePreview{}, err
	}
	count := len(presentation.Slides)
	if count == 0 {
		return filePreview{Pages: 0}, nil
	}
	page := min(req.page, count)
	targets, err := officeRelationships(z, part, "slide", "ppt/slides")
	if err != nil {
		return filePreview{}, errors.New("PPTX 幻灯片关系无效")
	}
	name := targets[relationshipID(presentation.Slides[page-1].Attributes, "id")]
	data, err = zipData(z, name, maxOfficeXML)
	if err != nil {
		return filePreview{}, errors.New("无法读取 PPTX 幻灯片")
	}
	result, err := officeBlocks(data, req, nil)
	result.Page, result.Pages = page, count
	return result, err
}

// officeBlocks 流式读取 Office XML，将段落和图片关系转换成预览块。
func officeBlocks(data []byte, req previewRequest, imageURL func(xml.StartElement) string) (filePreview, error) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	result := filePreview{}
	var paragraph strings.Builder
	textBytes := 0
	appendText := func() {
		if value := paragraph.String(); strings.TrimSpace(value) != "" {
			result.Blocks = append(result.Blocks, docBlock{Text: value})
			textBytes += len(value)
		}
		paragraph.Reset()
	}
	for {
		if len(result.Blocks) >= 1000 || textBytes >= maxTextPreview {
			result.Truncated = true
			return result, nil
		}
		if err := req.Err(); err != nil {
			return filePreview{}, err
		}
		token, err := decoder.Token()
		if err == io.EOF {
			return result, nil
		}
		if err != nil {
			return filePreview{}, err
		}
		switch token := token.(type) {
		case xml.StartElement:
			switch token.Name.Local {
			case "t":
				var piece string
				if err := decoder.DecodeElement(&piece, &token); err != nil {
					return filePreview{}, err
				}
				remaining := maxTextPreview - textBytes - paragraph.Len()
				if len(piece) > remaining {
					paragraph.Write(completeTextPrefix([]byte(piece[:remaining])))
					appendText()
					result.Truncated = true
					return result, nil
				}
				paragraph.WriteString(piece)
			case "br", "tab":
				if textBytes+paragraph.Len() < maxTextPreview {
					if token.Name.Local == "br" {
						paragraph.WriteByte('\n')
					} else {
						paragraph.WriteByte('\t')
					}
				}
			case "blip":
				if imageURL != nil {
					if image := imageURL(token); image != "" {
						appendText()
						result.Blocks = append(result.Blocks, docBlock{Image: image})
					}
				}
			}
		case xml.EndElement:
			if token.Name.Local == "p" {
				appendText()
			}
		}
	}
}
