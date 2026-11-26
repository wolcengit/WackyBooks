package main

// 本文件提供读者可见的收藏数据、清单操作和阅读进度持久化。
import (
	"database/sql"
	"net/http"
	"strings"
	"time"
)

func (s *Server) visibleReaderData(session Session) (ReaderData, error) {
	reader := ReaderData{
		Lists: make(map[string][]int64), Reading: make(map[int64]Reading),
		FavoriteDates: make(map[string]map[int64]*time.Time),
	}
	id := session.readerID
	var exists bool
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM readers WHERE public_key=? AND initialized=1)`, id).Scan(&exists); err != nil {
		return reader, err
	}
	if !exists {
		reader.Lists["我的收藏"] = []int64{}
	}
	if err := readRows(s.db, `SELECT l.name,f.book_id,f.added_ms FROM lists l
  LEFT JOIN favorites f ON f.list_id=l.id
  WHERE l.reader_id=(SELECT id FROM readers WHERE public_key=?) ORDER BY l.name,f.sequence`, func(rows *sql.Rows) error {
		var name string
		var book sql.NullInt64
		var added sql.NullInt64
		if err := rows.Scan(&name, &book, &added); err != nil {
			return err
		}
		if _, ok := reader.Lists[name]; !ok {
			reader.Lists[name] = []int64{}
			reader.FavoriteDates[name] = map[int64]*time.Time{}
		}
		if book.Valid {
			if _, allowed := s.accessibleBook(session, book.Int64); allowed {
				reader.Lists[name] = append(reader.Lists[name], book.Int64)
				var date *time.Time
				if added.Valid {
					value := fromMillis(added.Int64)
					date = &value
				}
				reader.FavoriteDates[name][book.Int64] = date
			}
		}
		return nil
	}, id); err != nil {
		return reader, err
	}
	err := readRows(s.db, `SELECT book_id,position,opened_ms,duration_ms,progress FROM reading WHERE reader_id=(SELECT id FROM readers WHERE public_key=?)`, func(rows *sql.Rows) error {
		var book int64
		var record Reading
		var opened int64
		if err := rows.Scan(&book, &record.Position, &opened, &record.DurationMS, &record.Progress); err != nil {
			return err
		}
		if _, allowed := s.accessibleBook(session, book); allowed {
			record.Opened = fromMillis(opened)
			reader.Reading[book] = record
		}
		return nil
	}, id)
	return reader, err
}

func (s *Server) changeList(w http.ResponseWriter, r *http.Request, session Session) {
	var in struct {
		Name, Action string
		Book         int64
	}
	if !decode(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 240 {
		fail(w, 400, "收藏清单名称无效")
		return
	}
	if in.Action == "toggle" {
		if _, ok := s.accessibleBook(session, in.Book); !ok {
			fail(w, 400, "书籍不存在")
			return
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		storageError(w, err)
		return
	}
	defer tx.Rollback()
	id, err := ensureReader(tx, session.readerID)
	if err != nil {
		storageError(w, err)
		return
	}
	result, err := tx.Exec(`UPDATE readers SET initialized=1 WHERE id=? AND initialized=0`, id)
	if err != nil {
		storageError(w, err)
		return
	}
	if inserted, _ := result.RowsAffected(); inserted != 0 {
		if _, err = tx.Exec(`INSERT INTO lists (reader_id,name) VALUES (?,?)`, id, "我的收藏"); err != nil {
			storageError(w, err)
			return
		}
	}
	switch in.Action {
	case "create":
		result, err = tx.Exec(`INSERT OR IGNORE INTO lists (reader_id,name) VALUES (?,?)`, id, in.Name)
		if err == nil {
			if added, _ := result.RowsAffected(); added == 0 {
				fail(w, 400, "收藏清单已存在")
				return
			}
		}
	case "delete":
		_, err = tx.Exec(`DELETE FROM lists WHERE reader_id=? AND name=?`, id, in.Name)
	case "toggle":
		var listID int64
		err = tx.QueryRow(`SELECT id FROM lists WHERE reader_id=? AND name=?`, id, in.Name).Scan(&listID)
		if err == sql.ErrNoRows {
			fail(w, 400, "收藏清单不存在")
			return
		}
		if err != nil {
			break
		}
		result, err = tx.Exec(`DELETE FROM favorites WHERE list_id=? AND book_id=?`, listID, in.Book)
		if err == nil {
			if removed, _ := result.RowsAffected(); removed == 0 {
				_, err = tx.Exec(`INSERT INTO favorites (list_id,book_id,added_ms,sequence)
 SELECT ?,?,?,COALESCE(MAX(sequence)+1,0) FROM favorites WHERE list_id=?`, listID, in.Book, time.Now().UnixMilli(), listID)
			}
		}
	default:
		fail(w, 400, "收藏操作无效")
		return
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		storageError(w, err)
		return
	}
	reader, err := s.visibleReaderData(session)
	if err != nil {
		storageError(w, err)
		return
	}
	reply(w, reader.Lists)
}

// saveReading 校验书籍权限和阅读位置，并原子更新进度与阅读时长。
func (s *Server) saveReading(w http.ResponseWriter, r *http.Request, session Session) {
	var in struct {
		ID        int64
		Position  int
		Progress  *int
		ElapsedMS int64
	}
	if !decode(w, r, &in) {
		return
	}
	book, exists := s.accessibleBook(session, in.ID)
	limit := book.Pages
	if book.Format == "epub" {
		limit = len(book.chapters)
	}
	if !exists || in.Position < 0 || (limit > 0 && in.Position >= limit) || in.ElapsedMS < 0 || in.ElapsedMS > 30000 ||
		(in.Progress != nil && (*in.Progress < 0 || *in.Progress > 100)) {
		fail(w, 400, "阅读记录无效")
		return
	}
	opened := time.Now().UnixMilli()
	record := Reading{Opened: fromMillis(opened)}
	// Cap accrual at the time since the last write, including when several tabs report the same book.
	tx, err := s.db.Begin()
	if err != nil {
		storageError(w, err)
		return
	}
	defer tx.Rollback()
	id, err := ensureReader(tx, session.readerID)
	if err != nil {
		storageError(w, err)
		return
	}
	err = tx.QueryRow(`INSERT INTO reading (reader_id,book_id,position,opened_ms,duration_ms,progress) VALUES (?,?,?,?,0,?)
 ON CONFLICT(reader_id,book_id) DO UPDATE SET position=excluded.position,
 duration_ms=reading.duration_ms+min(?,max(0,excluded.opened_ms-reading.opened_ms)),
 opened_ms=max(reading.opened_ms,excluded.opened_ms),progress=coalesce(excluded.progress,reading.progress)
 RETURNING position,opened_ms,duration_ms,progress`, id, in.ID, in.Position, opened, in.Progress, in.ElapsedMS).
		Scan(&record.Position, &opened, &record.DurationMS, &record.Progress)
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		storageError(w, err)
		return
	}
	record.Opened = fromMillis(opened)
	reply(w, record)
}
