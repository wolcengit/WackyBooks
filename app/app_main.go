package main

// 本文件定义应用状态、共享响应工具、API 路由和进程启动入口。
import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"log"
	"maps"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Library 表示一项书库配置；内部 key 用于稳定识别其中的书籍文件。
type Library struct {
	ID              int64
	Name, Directory string
	key             string
}

// Reading 保存读者最近一次打开位置和累计阅读时长。
type Reading struct {
	Position   int
	Opened     time.Time
	DurationMS int64
	Progress   *int
}

// ReaderData 是当前读者可见的收藏清单和阅读记录。
type ReaderData struct {
	Reading       map[int64]Reading
	Lists         map[string][]int64
	FavoriteDates map[string]map[int64]*time.Time
}

// Session 是保存在服务内存中的已认证会话。
type Session struct {
	Username string
	Admin    bool
	Source   string
	Expires  time.Time
	readerID string
}

// State 保存需要与 SQLite 配置表同步的应用设置。
type State struct {
	Password  string
	Libraries []Library
	LDAP      LDAPConfig
	OIDC      OIDCConfig
	Users     map[string]User
}

// Server 汇总 API 处理所需的状态、存储连接、会话和扫描索引。
type Server struct {
	mu          sync.Mutex
	state       State
	db          *sql.DB
	sessions    map[string]Session
	oidcFlows   map[string]oidcFlow
	books       map[int64]Book
	directories map[int64][]string
}

func token() string     { return hex.EncodeToString(randBytes()) }
func randBytes() []byte { b := make([]byte, 32); rand.Read(b); return b }
func passwordHash(password, salt string) string {
	key := pbkdf(password, salt)
	return salt + ":" + hex.EncodeToString(key)
}
func pbkdf(password, salt string) []byte {
	key, err := pbkdf2Key(password, []byte(salt))
	if err != nil {
		panic(err)
	}
	return key
}
func checkPassword(password, stored string) bool {
	salt, _, ok := strings.Cut(stored, ":")
	return ok && subtle.ConstantTimeCompare([]byte(stored), []byte(passwordHash(password, salt))) == 1
}

// update 先在副本上应用设置，再提交数据库事务，避免失败时污染内存状态。
func (s *Server) update(w http.ResponseWriter, change func(*State) error) bool {
	next := s.state
	next.Libraries = slices.Clone(next.Libraries)
	next.Users = maps.Clone(next.Users)
	for id, user := range next.Users {
		user.LibraryIDs = slices.Clone(user.LibraryIDs)
		next.Users[id] = user
	}
	if err := change(&next); err != nil {
		fail(w, 400, err.Error())
		return false
	}
	tx, err := s.db.Begin()
	if err == nil {
		defer tx.Rollback()
		err = writeSettings(tx, next)
		if err == nil {
			err = tx.Commit()
		}
	}
	if err != nil {
		storageError(w, err)
		return false
	}
	s.state = next
	return true
}

func storageError(w http.ResponseWriter, err error) {
	log.Print(err)
	fail(w, 500, "无法读取或保存数据")
}

func reply(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(data)
}
func fail(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}
func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536)).Decode(dst); err != nil {
		fail(w, 400, "请求数据无效")
		return false
	}
	return true
}
func cookie(w http.ResponseWriter, r *http.Request, value string, age int, secure bool) {
	http.SetCookie(w, &http.Cookie{Name: "wacky_session", Value: value, Path: "/", HttpOnly: true, Secure: r.TLS != nil || secure, SameSite: http.SameSiteStrictMode, MaxAge: age})
}

// api 统一执行来源校验、会话认证、管理员权限检查和接口分发。
func (s *Server) api(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		fail(w, 403, "请求来源无效")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, _ := r.Cookie("wacky_session")
	var session Session
	if c != nil {
		session = s.sessions[c.Value]
	}
	authenticated := time.Now().Before(session.Expires)
	if r.Method == "GET" && r.URL.Path == "/api/session" {
		reply(w, map[string]any{"authenticated": authenticated, "setup": s.state.Password == "",
			"ldapEnabled": s.state.LDAP.Enabled, "oidcEnabled": s.state.OIDC.Enabled,
			"username": session.Username, "source": session.Source, "admin": authenticated && session.Admin})
		return
	}
	if r.URL.Path == "/api/oidc/login" && r.Method == http.MethodGet {
		s.oidcLogin(w, r)
		return
	}
	if r.URL.Path == "/api/oidc/callback" && r.Method == http.MethodGet {
		s.oidcCallback(w, r)
		return
	}
	if r.Method == "POST" && r.URL.Path == "/api/login" {
		s.login(w, r)
		return
	}
	if !authenticated {
		fail(w, 401, "请先登录")
		return
	}
	switch r.URL.Path {
	case "/api/library", "/api/library/scan", "/api/directories", "/api/scan", "/api/ldap", "/api/ldap/test", "/api/oidc", "/api/oidc/test", "/api/users", "/api/users/access":
		if !session.Admin {
			fail(w, 403, "此操作需要管理员权限")
			return
		}
	}
	switch r.Method + " " + r.URL.Path {
	case "POST /api/logout":
		delete(s.sessions, c.Value)
		cookie(w, r, "", -1, false)
		reply(w, true)
	case "GET /api/ldap", "POST /api/ldap", "POST /api/ldap/test":
		s.ldapSettings(w, r)
	case "GET /api/oidc", "POST /api/oidc", "POST /api/oidc/test":
		s.oidcSettings(w, r)
	case "GET /api/directories":
		browseDirectories(w, r)
	case "GET /api/users", "POST /api/users", "POST /api/users/access":
		s.userManagement(w, r)
	case "POST /api/password":
		if session.Source == "oidc" {
			fail(w, 403, "请在单点登录服务中管理账户密码")
			return
		}
		var in struct{ Current, Password string }
		if !decode(w, r, &in) {
			return
		}
		credential := s.state.Password
		if !session.Admin {
			user := s.state.Users[session.readerID]
			if user.Source != "local" {
				fail(w, 403, "请在 LDAP 目录中管理账户密码")
				return
			}
			credential = user.PasswordHash
		}
		if len(in.Current) > 72 || !checkPassword(in.Current, credential) {
			fail(w, 400, "当前密码错误")
			return
		}
		if len(in.Password) < 8 || len(in.Password) > 72 {
			fail(w, 400, "新密码需要 8 至 72 字节")
			return
		}
		if s.update(w, func(next *State) error {
			hash := passwordHash(in.Password, token())
			if session.Admin {
				next.Password = hash
			} else {
				user := next.Users[session.readerID]
				user.PasswordHash = hash
				next.Users[session.readerID] = user
			}
			return nil
		}) {
			for id, other := range s.sessions {
				if id != c.Value && other.Admin == session.Admin && other.readerID == session.readerID {
					delete(s.sessions, id)
				}
			}
			reply(w, true)
		}
	case "GET /api/catalog", "POST /api/scan":
		if r.Method == "POST" || s.books == nil {
			if err := s.scan(); err != nil {
				storageError(w, err)
				return
			}
		}
		s.catalog(w, session)
	case "GET /api/search":
		if s.books == nil {
			if err := s.scan(); err != nil {
				storageError(w, err)
				return
			}
		}
		s.searchBooks(w, r, session)
	case "POST /api/library", "DELETE /api/library":
		s.library(w, r)
	case "POST /api/library/scan":
		s.rescanLibrary(w, r)
	case "POST /api/list":
		s.changeList(w, r, session)
	case "POST /api/reading":
		s.saveReading(w, r, session)
	default:
		if (r.Method == "GET" || r.Method == "HEAD") && strings.HasPrefix(r.URL.Path, "/api/book/") {
			id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
			book, ok := s.accessibleBook(session, id)
			if !ok {
				fail(w, 404, "书籍不存在")
				return
			}
			s.mu.Unlock()
			defer s.mu.Lock()
			serveBook(w, r, book)
			return
		}
		fail(w, 404, "接口不存在")
	}
}

// main 初始化配置与数据库，注册 API 路由，并启动 HTTP 服务。
func main() {
	dir := os.Getenv("WACKY_DATA")
	if dir == "" {
		dir = "data"
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		log.Fatal(err)
	}
	db, err := openStorage(dir)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	state, err := loadSettings(db)
	if err != nil {
		log.Fatal(err)
	}
	s := &Server{db: db, state: state, sessions: map[string]Session{}, oidcFlows: map[string]oidcFlow{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/book/{id}/assets/{resource...}", func(w http.ResponseWriter, r *http.Request) {
		r.SetPathValue("action", "assets")
		s.api(w, r)
	})
	mux.HandleFunc("/api/book/{id}/{action}", s.api)
	mux.HandleFunc("/api/", s.api)
	mux.Handle("/", frontend())
	addr := os.Getenv("WACKY_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	log.Printf("WackyBooks http://%s", addr)
	server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(server.ListenAndServe())
}
func bookKey(library, path string) string {
	sum := sha256.Sum256([]byte(library + "\x00" + path))
	return hex.EncodeToString(sum[:16])
}
