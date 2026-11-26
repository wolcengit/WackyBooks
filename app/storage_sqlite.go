package main

// 本文件定义 SQLite 表结构及应用配置、用户、授权和读者记录的存取逻辑。
import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// schema 是首次启动时直接创建的当前数据库结构。
const schema = `
CREATE TABLE settings (id INTEGER PRIMARY KEY CHECK(id=1), password TEXT NOT NULL, ldap TEXT NOT NULL, oidc TEXT NOT NULL);
CREATE TABLE libraries (id INTEGER PRIMARY KEY, public_key TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL, directory TEXT NOT NULL, sequence INTEGER NOT NULL);
CREATE TABLE readers (id INTEGER PRIMARY KEY, public_key TEXT NOT NULL UNIQUE, initialized INTEGER NOT NULL DEFAULT 0);
CREATE TABLE users (id INTEGER PRIMARY KEY REFERENCES readers(id), username TEXT NOT NULL,
    source TEXT NOT NULL, password_hash TEXT NOT NULL, disabled INTEGER NOT NULL);
CREATE TABLE books (id INTEGER PRIMARY KEY, public_key TEXT NOT NULL UNIQUE,
    library_id INTEGER REFERENCES libraries(id) ON DELETE SET NULL, metadata TEXT NOT NULL DEFAULT '{}',
    search_text TEXT NOT NULL DEFAULT '', title TEXT NOT NULL DEFAULT '', format TEXT NOT NULL DEFAULT '',
    active INTEGER NOT NULL DEFAULT 0);
CREATE INDEX books_library ON books(library_id, active);
CREATE TABLE grants (id INTEGER PRIMARY KEY, reader_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    library_id INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE, UNIQUE(reader_id, library_id));
CREATE TABLE reading (id INTEGER PRIMARY KEY, reader_id INTEGER NOT NULL REFERENCES readers(id),
    book_id INTEGER NOT NULL REFERENCES books(id), position INTEGER NOT NULL, opened_ms INTEGER NOT NULL,
    duration_ms INTEGER NOT NULL DEFAULT 0, progress INTEGER, UNIQUE(reader_id, book_id));
CREATE TABLE lists (id INTEGER PRIMARY KEY, reader_id INTEGER NOT NULL REFERENCES readers(id),
    name TEXT NOT NULL, UNIQUE(reader_id, name));
CREATE TABLE favorites (id INTEGER PRIMARY KEY, list_id INTEGER NOT NULL REFERENCES lists(id) ON DELETE CASCADE,
    book_id INTEGER NOT NULL REFERENCES books(id), added_ms INTEGER, sequence INTEGER NOT NULL, UNIQUE(list_id, book_id));
CREATE INDEX favorites_book ON favorites(book_id, list_id);
PRAGMA user_version=4;
`

// openStorage 创建权限受限的数据库文件并启用外键与繁忙等待。
func openStorage(dir string) (*sql.DB, error) {
	path, err := filepath.Abs(filepath.Join(dir, "state.sqlite"))
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = file.Chmod(0600); err != nil {
		file.Close()
		return nil, err
	}
	if err = file.Close(); err != nil {
		return nil, err
	}
	dsn := url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)"}
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err = initializeStorage(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// initializeStorage 仅创建空数据库结构，不导入旧格式数据。
func initializeStorage(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	switch version {
	case 4:
		return nil
	case 0:
		if _, err := tx.Exec(schema); err != nil {
			return err
		}
		if err := writeGlobalSettings(tx, State{}); err != nil {
			return err
		}
		return tx.Commit()
	default:
		return fmt.Errorf("不支持的数据库版本 %d", version)
	}
}

func writeGlobalSettings(tx *sql.Tx, state State) error {
	ldap, err := json.Marshal(state.LDAP)
	if err != nil {
		return err
	}
	oidc, err := json.Marshal(state.OIDC)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO settings VALUES (1,?,?,?) ON CONFLICT(id) DO UPDATE SET
 password=excluded.password,ldap=excluded.ldap,oidc=excluded.oidc`, state.Password, string(ldap), string(oidc)); err != nil {
		return err
	}
	return nil
}

func writeSettings(tx *sql.Tx, state State) error {
	if err := writeGlobalSettings(tx, state); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE TEMP TABLE desired_users (id INTEGER PRIMARY KEY);
 CREATE TEMP TABLE desired_grants (reader_id INTEGER, library_id INTEGER, UNIQUE(reader_id,library_id));
 UPDATE libraries SET sequence=-1`); err != nil {
		return err
	}
	for i := range state.Libraries {
		lib := &state.Libraries[i]
		if lib.ID == 0 {
			lib.key = token()
			if err := tx.QueryRow(`INSERT INTO libraries (public_key,name,directory,sequence) VALUES (?,?,?,?) RETURNING id`,
				lib.key, lib.Name, lib.Directory, i).Scan(&lib.ID); err != nil {
				return err
			}
		} else if _, err := tx.Exec(`UPDATE libraries SET name=?,sequence=? WHERE id=?`, lib.Name, i, lib.ID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM libraries WHERE sequence=-1`); err != nil {
		return err
	}
	for key, user := range state.Users {
		id, err := ensureReader(tx, key)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO users VALUES (?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET username=excluded.username,
 source=excluded.source,password_hash=excluded.password_hash,disabled=excluded.disabled`, id, user.Username, user.Source, user.PasswordHash, user.Disabled); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO desired_users VALUES (?)`, id); err != nil {
			return err
		}
		for _, lib := range user.LibraryIDs {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO desired_grants (reader_id,library_id) SELECT ?,id FROM libraries WHERE id=?`, id, lib); err != nil {
				return err
			}
		}
	}
	_, err := tx.Exec(`DELETE FROM users WHERE id NOT IN (SELECT id FROM desired_users);
 DELETE FROM grants WHERE (reader_id,library_id) NOT IN (SELECT reader_id,library_id FROM desired_grants);
 INSERT OR IGNORE INTO grants (reader_id,library_id) SELECT reader_id,library_id FROM desired_grants;
 DROP TABLE desired_grants; DROP TABLE desired_users`)
	return err
}

func ensureReader(tx *sql.Tx, key string) (int64, error) {
	if _, err := tx.Exec(`INSERT INTO readers (public_key) VALUES (?) ON CONFLICT(public_key) DO NOTHING`, key); err != nil {
		return 0, err
	}
	var id int64
	err := tx.QueryRow(`SELECT id FROM readers WHERE public_key=?`, key).Scan(&id)
	return id, err
}

func readRows(db *sql.DB, query string, read func(*sql.Rows) error, args ...any) error {
	rows, err := db.Query(query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := read(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func loadSettings(db *sql.DB) (State, error) {
	state := State{Users: map[string]User{}}
	var ldap, oidc string
	if err := db.QueryRow(`SELECT password,ldap,oidc FROM settings WHERE id=1`).Scan(&state.Password, &ldap, &oidc); err != nil {
		return state, err
	}
	if err := json.Unmarshal([]byte(ldap), &state.LDAP); err != nil {
		return state, err
	}
	if err := json.Unmarshal([]byte(oidc), &state.OIDC); err != nil {
		return state, err
	}
	if err := readRows(db, `SELECT id,name,directory,public_key FROM libraries ORDER BY sequence`, func(rows *sql.Rows) error {
		var lib Library
		if err := rows.Scan(&lib.ID, &lib.Name, &lib.Directory, &lib.key); err != nil {
			return err
		}
		state.Libraries = append(state.Libraries, lib)
		return nil
	}); err != nil {
		return state, err
	}
	if err := readRows(db, `SELECT r.public_key,u.username,u.source,u.password_hash,u.disabled FROM users u JOIN readers r ON r.id=u.id`, func(rows *sql.Rows) error {
		var id string
		var user User
		if err := rows.Scan(&id, &user.Username, &user.Source, &user.PasswordHash, &user.Disabled); err != nil {
			return err
		}
		state.Users[id] = user
		return nil
	}); err != nil {
		return state, err
	}
	err := readRows(db, `SELECT r.public_key,g.library_id FROM grants g JOIN readers r ON r.id=g.reader_id ORDER BY g.library_id`, func(rows *sql.Rows) error {
		var id string
		var library int64
		if err := rows.Scan(&id, &library); err != nil {
			return err
		}
		user := state.Users[id]
		user.LibraryIDs = append(user.LibraryIDs, library)
		state.Users[id] = user
		return nil
	})
	return state, err
}

func fromMillis(ms int64) time.Time { return time.UnixMilli(ms).UTC() }
