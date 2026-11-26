package main

// 本文件以分页方式读取 CSV、TSV、XLSX 和 Parquet 表格数据。
import (
	"bytes"
	"encoding/binary"
	"encoding/csv"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/parquet-go/parquet-go"
	"github.com/xuri/excelize/v2"
)

const (
	previewPageSize = 50
	maxTableColumns = 100
	maxCellBytes    = 1024
	maxDelimitedRow = 1 << 20
)

// previewTable 描述一个工作表或数据集当前页的列、行及翻页状态。
type previewTable struct {
	Sheet     int        `json:"sheet"`
	Sheets    []string   `json:"sheets,omitempty"`
	Headers   []string   `json:"headers"`
	Rows      [][]string `json:"rows"`
	Page      int        `json:"page"`
	PageSize  int        `json:"pageSize"`
	TotalRows *int64     `json:"totalRows,omitempty"`
	More      bool       `json:"more"`
}

func (t *previewTable) appendRow(row []string) {
	if len(row) > maxTableColumns {
		row = row[:maxTableColumns]
	}
	for i, value := range row {
		row[i] = previewCell(value)
	}
	if row == nil {
		row = []string{}
	}
	t.Rows = append(t.Rows, row)
}

func previewCell(value string) string {
	if len(value) > maxCellBytes {
		return string(completeTextPrefix([]byte(value[:maxCellBytes]))) + "…"
	}
	return value
}

func (t *previewTable) columnHeaders() {
	for _, row := range t.Rows {
		for len(t.Headers) < len(row) {
			name, _ := excelize.ColumnNumberToName(len(t.Headers) + 1)
			t.Headers = append(t.Headers, name)
		}
	}
	for i, value := range t.Headers {
		t.Headers[i] = previewCell(value)
	}
}

type delimitedSource struct {
	file      *os.File
	remaining int64
	end       int64
}

func (s *delimitedSource) Read(p []byte) (int, error) {
	if s.remaining <= 0 {
		return 0, errors.New("单条记录超过 1 MiB")
	}
	n, err := s.file.Read(p[:min(int64(len(p)), s.remaining)])
	s.remaining -= int64(n)
	s.end += int64(n)
	return n, err
}

// collectTable 统一截取指定页，并在遇到下一条记录时标记仍有后续数据。
func collectTable(req previewRequest, headers []string, next func() ([]string, error)) (*previewTable, error) {
	table := &previewTable{Headers: headers, Rows: [][]string{}, Page: req.page, PageSize: previewPageSize}
	offset := int64(req.page-1) * previewPageSize
	for i := int64(0); i <= offset+previewPageSize; i++ {
		if err := req.Err(); err != nil {
			return nil, err
		}
		row, err := next()
		if err == io.EOF {
			table.TotalRows = &i
			break
		}
		if err != nil {
			return nil, err
		}
		if i == offset+previewPageSize {
			table.More = true
		} else if i >= offset {
			table.appendRow(row)
		}
	}
	table.columnHeaders()
	return table, nil
}

func readDelimitedPreview(f *os.File, b Book, req previewRequest) (filePreview, error) {
	source := &delimitedSource{file: f, remaining: maxDelimitedRow}
	var bom [3]byte
	if n, _ := f.ReadAt(bom[:], 0); n == 3 && bom == [3]byte{0xef, 0xbb, 0xbf} {
		if _, err := f.Seek(3, io.SeekStart); err != nil {
			return filePreview{}, err
		}
	}
	reader := csv.NewReader(source)
	reader.FieldsPerRecord = -1
	if b.Format == "tsv" {
		reader.Comma = '\t'
	}
	next := func() ([]string, error) {
		source.remaining = maxDelimitedRow - (source.end - reader.InputOffset())
		row, err := reader.Read()
		return row, err
	}
	headers, err := next()
	if err != nil && err != io.EOF {
		return filePreview{}, err
	}
	if len(headers) > maxTableColumns {
		headers = headers[:maxTableColumns]
	}
	table, err := collectTable(req, append([]string{}, headers...), next)
	return filePreview{Table: table}, err
}

func readExcelPreview(f *os.File, _ Book, req previewRequest) (filePreview, error) {
	book, err := excelize.OpenReader(f, excelize.Options{UnzipSizeLimit: 256 << 20, UnzipXMLSizeLimit: 8 << 20})
	if err != nil {
		return filePreview{}, errors.New("无法读取 XLSX 工作簿")
	}
	defer book.Close()
	sheets := book.GetSheetList()
	if len(sheets) == 0 {
		return filePreview{}, errors.New("工作簿没有工作表")
	}
	sheet := req.sheet
	if sheet >= len(sheets) {
		sheet = 0
		req.page = 1
	}
	rows, err := book.Rows(sheets[sheet])
	if err != nil {
		return filePreview{}, errors.New("无法读取工作表")
	}
	defer rows.Close()
	table, err := collectTable(req, []string{}, func() ([]string, error) {
		if !rows.Next() {
			if err := rows.Error(); err != nil {
				return nil, err
			}
			return nil, io.EOF
		}
		return rows.Columns()
	})
	if err != nil {
		return filePreview{}, errors.New("无法读取工作表行")
	}
	table.Sheet = sheet
	table.Sheets = sheets
	return filePreview{Table: table}, nil
}

func readParquetPreview(f *os.File, _ Book, req previewRequest) (filePreview, error) {
	info, err := f.Stat()
	if err != nil {
		return filePreview{}, err
	}
	var footer [8]byte
	if info.Size() < 12 {
		return filePreview{}, errors.New("Parquet 文件不完整")
	}
	_, err = f.ReadAt(footer[:], info.Size()-8)
	footerSize := int64(binary.LittleEndian.Uint32(footer[:4]))
	if err != nil || !bytes.Equal(footer[4:], []byte("PAR1")) || footerSize > min(16<<20, info.Size()-12) {
		return filePreview{}, errors.New("Parquet 文件元数据无效或过大")
	}
	file, err := parquet.OpenFile(f, info.Size(), parquet.SkipPageIndex(true), parquet.SkipBloomFilters(true))
	if err != nil {
		return filePreview{}, err
	}
	total := file.NumRows()
	columns := file.Schema().Columns()
	table := &previewTable{Headers: []string{}, Rows: [][]string{}, Page: req.page, PageSize: previewPageSize, TotalRows: &total}
	for _, column := range columns[:min(len(columns), maxTableColumns)] {
		table.Headers = append(table.Headers, previewCell(strings.Join(column, ".")))
	}
	offset := int64(req.page-1) * previewPageSize
	if offset >= total {
		return filePreview{Table: table}, nil
	}
	rows := parquet.MultiRowGroup(file.RowGroups()...).Rows()
	defer rows.Close()
	if err := rows.SeekToRow(offset); err != nil {
		return filePreview{}, err
	}
	batch := make([]parquet.Row, 1)
	for len(table.Rows) < previewPageSize {
		if err := req.Err(); err != nil {
			return filePreview{}, err
		}
		n, err := rows.ReadRows(batch)
		if n > 0 {
			row := make([]string, len(table.Headers))
			batch[0].Range(func(column int, values []parquet.Value) bool {
				if column >= len(row) {
					return false
				}
				var cell strings.Builder
				for _, value := range values {
					if value.IsNull() {
						continue
					}
					if cell.Len() > 0 {
						cell.WriteString(", ")
					}
					switch value.Kind() {
					case parquet.ByteArray, parquet.FixedLenByteArray:
						data := value.ByteArray()
						cell.Write(data[:min(len(data), maxCellBytes+1)])
					default:
						cell.WriteString(value.String())
					}
					if cell.Len() > maxCellBytes {
						break
					}
				}
				row[column] = cell.String()
				return true
			})
			table.appendRow(row)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return filePreview{}, err
		}
	}
	table.More = offset+int64(len(table.Rows)) < total
	return filePreview{Table: table}, nil
}
