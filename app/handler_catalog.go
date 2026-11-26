package main

// 本文件负责书目索引、可见书库筛选、目录响应和全文搜索。
import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// indexBooks 在一个事务内更新扫描结果，并保留离线书籍的历史记录。
func (s *Server) indexBooks(books map[string]Book) error {
	return s.indexBookSet(books, 0)
}

// indexBookSet 按范围更新扫描结果，libraryID 为零时更新全部书库。
func (s *Server) indexBookSet(books map[string]Book, libraryID int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if libraryID == 0 {
		_, err = tx.Exec(`UPDATE books SET active=0 WHERE active=1`)
	} else {
		_, err = tx.Exec(`UPDATE books SET active=0 WHERE active=1 AND library_id=?`, libraryID)
	}
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO books (public_key,library_id,metadata,search_text,title,format,active)
 VALUES (?,?,?,?,?,?,1)
 ON CONFLICT(public_key) DO UPDATE SET library_id=excluded.library_id,metadata=excluded.metadata,search_text=excluded.search_text,
 title=excluded.title,format=excluded.format,active=1 RETURNING id`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for key, book := range books {
		metadata, err := json.Marshal(struct {
			Metadata
			Path, Format string
			Added        time.Time
			Pages        int
		}{book.Metadata, book.Path, book.Format, book.Added, book.Pages})
		if err != nil {
			return err
		}
		terms := []string{book.Title, book.Path, book.Publisher, book.Format, book.Description, book.Language, book.Identifier, book.Date}
		terms = append(terms, book.Authors...)
		terms = append(terms, book.Subjects...)
		if err = stmt.QueryRow(key, book.Library, string(metadata), strings.ToLower(strings.Join(terms, "\n")), strings.ToLower(book.Title), book.Format).Scan(&book.ID); err != nil {
			return err
		}
		books[key] = book
	}
	return tx.Commit()
}

// 管理员可看全部书籍，普通读者仅能看获授权书库中的书籍。
const visibleBooks = ` FROM books b WHERE b.active=1 AND (? OR EXISTS
 (SELECT 1 FROM grants g JOIN users u ON u.id=g.reader_id
  WHERE g.reader_id=(SELECT id FROM readers WHERE public_key=?) AND g.library_id=b.library_id AND u.disabled=0))`
const selectBooks = `SELECT b.id,b.library_id,b.metadata,(SELECT COUNT(DISTINCT l.reader_id) FROM favorites f JOIN lists l ON l.id=f.list_id WHERE f.book_id=b.id) AS favorite_count`

func (s *Server) listBooks(query string, args ...any) ([]Book, error) {
	books := []Book{}
	err := readRows(s.db, query, func(rows *sql.Rows) error {
		var meta string
		var count int
		var id, libraryID int64
		var book Book
		if err := rows.Scan(&id, &libraryID, &meta, &count); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(meta), &book); err != nil {
			return err
		}
		book.ID, book.Library, book.FavoriteCount = id, libraryID, count
		books = append(books, book)
		return nil
	}, args...)
	return books, err
}

func (s *Server) catalog(w http.ResponseWriter, session Session) {
	books, err := s.listBooks(selectBooks+visibleBooks, session.Admin, session.readerID)
	if err != nil {
		storageError(w, err)
		return
	}
	reader, err := s.visibleReaderData(session)
	if err != nil {
		storageError(w, err)
		return
	}
	libraries := []Library{}
	directories := make(map[int64][]string)
	for _, library := range s.state.Libraries {
		if !s.canAccessLibrary(session, library.ID) {
			continue
		}
		if !session.Admin {
			library.Directory = ""
		}
		libraries = append(libraries, library)
		directories[library.ID] = s.directories[library.ID]
	}
	reply(w, map[string]any{"libraries": libraries, "books": books, "directories": directories,
		"reading": reader.Reading, "lists": reader.Lists, "favoriteDates": reader.FavoriteDates})
}

func (s *Server) searchBooks(w http.ResponseWriter, r *http.Request, session Session) {
	params := r.URL.Query()
	words := strings.Fields(strings.ToLower(params.Get("q")))
	if len(params.Get("q")) > 1024 || len(words) > 24 {
		fail(w, 400, "搜索关键词过长")
		return
	}
	query := selectBooks + visibleBooks
	args := []any{session.Admin, session.readerID}
	for _, word := range words {
		query += ` AND instr(b.search_text, ?) > 0`
		args = append(args, word)
	}
	if library := params.Get("library"); library != "" {
		query += ` AND b.library_id=?`
		args = append(args, library)
	}
	if format := params.Get("format"); format != "" {
		query += ` AND b.format=?`
		args = append(args, format)
	}
	if params.Get("sort") == "favorites" {
		query += ` ORDER BY favorite_count DESC, b.title, b.public_key`
	} else {
		query += ` ORDER BY b.title,b.public_key`
	}
	offset, err := strconv.Atoi(params.Get("offset"))
	if params.Get("offset") != "" && (err != nil || offset < 0) {
		fail(w, 400, "搜索页码无效")
		return
	}
	const pageSize = 48
	books, err := s.listBooks(query+` LIMIT ? OFFSET ?`, append(args, pageSize+1, offset)...)
	if err != nil {
		storageError(w, err)
		return
	}
	more := len(books) > pageSize
	if more {
		books = books[:pageSize]
	}
	reply(w, map[string]any{"books": books, "more": more})
}
