package main

// 本文件管理账户、用户书库授权以及书籍访问控制判断。
import (
	"errors"
	"net/http"
	"slices"
	"sort"
	"strings"
	"unicode"
)

// User 保存本地或 LDAP 读者账户及授权书库。
type User struct {
	Username     string
	Source       string
	PasswordHash string
	Disabled     bool
	LibraryIDs   []int64
}

type userInfo struct {
	ID, Username, Source string
	Disabled             bool
	LibraryIDs           []int64
}

func (state *State) localUser(username string) (string, User) {
	for id, user := range state.Users {
		if user.Source == "local" && strings.EqualFold(user.Username, username) {
			return id, user
		}
	}
	return "", User{}
}

func (s *Server) canAccessLibrary(session Session, libraryID int64) bool {
	if session.Admin {
		return true
	}
	user, exists := s.state.Users[session.readerID]
	return exists && !user.Disabled && slices.Contains(user.LibraryIDs, libraryID)
}

// accessibleBook 同时检查书籍是否存在以及当前会话是否有权访问其书库。
func (s *Server) accessibleBook(session Session, id int64) (Book, bool) {
	book, exists := s.books[id]
	return book, exists && s.canAccessLibrary(session, book.Library)
}

func (s *Server) registerLDAP(w http.ResponseWriter, id, username string) bool {
	user, exists := s.state.Users[id]
	if exists && user.Disabled {
		fail(w, 403, "账户已停用")
		return false
	}
	if exists && user.Username == username {
		return true
	}
	user.Username, user.Source = username, "ldap"
	return s.update(w, func(next *State) error {
		next.putUser(id, user)
		return nil
	})
}

func (state *State) putUser(id string, user User) {
	if state.Users == nil {
		state.Users = make(map[string]User)
	}
	state.Users[id] = user
}

func (s *Server) userManagement(w http.ResponseWriter, r *http.Request) {
	switch r.Method + " " + r.URL.Path {
	case "GET /api/users":
		users := make([]userInfo, 0, len(s.state.Users))
		for id, user := range s.state.Users {
			users = append(users, userInfo{id, user.Username, user.Source, user.Disabled, user.LibraryIDs})
		}
		sort.Slice(users, func(i, j int) bool {
			if users[i].Username == users[j].Username {
				return users[i].ID < users[j].ID
			}
			return users[i].Username < users[j].Username
		})
		reply(w, users)
	case "POST /api/users":
		var in struct {
			ID, Username, Password string
			Disabled               bool
		}
		if !decode(w, r, &in) {
			return
		}
		user, exists := s.state.Users[in.ID]
		if in.ID == "" {
			in.Username = strings.TrimSpace(in.Username)
			if in.Username == "" || strings.TrimSpace(in.Username) != in.Username || strings.Contains(in.Username, "@") ||
				len(in.Username) > 128 || strings.EqualFold(in.Username, "Admin") ||
				strings.ContainsFunc(in.Username, unicode.IsControl) {
				fail(w, 400, "用户名无效或已保留")
				return
			}
			for _, existing := range s.state.Users {
				if existing.Source == "local" && strings.EqualFold(existing.Username, in.Username) {
					fail(w, 400, "用户名已存在")
					return
				}
			}
			in.ID = "local:" + token()
			user = User{Username: in.Username, Source: "local"}
		} else if !exists {
			fail(w, 404, "用户不存在")
			return
		}
		if !exists || in.Password != "" {
			if user.Source != "local" || len(in.Password) < 8 || len(in.Password) > 72 {
				fail(w, 400, "本地密码需要 8 至 72 字节")
				return
			}
			user.PasswordHash = passwordHash(in.Password, token())
		}
		user.Disabled = in.Disabled
		if s.update(w, func(next *State) error { next.putUser(in.ID, user); return nil }) {
			if user.Disabled || in.Password != "" {
				for id, session := range s.sessions {
					if session.readerID == in.ID {
						delete(s.sessions, id)
					}
				}
			}
			reply(w, true)
		}
	case "POST /api/users/access":
		var in struct {
			ID         string
			LibraryIDs []int64
		}
		if !decode(w, r, &in) {
			return
		}
		if s.update(w, func(next *State) error {
			user, exists := next.Users[in.ID]
			if !exists {
				return errors.New("用户不存在")
			}
			available := make(map[int64]bool, len(next.Libraries))
			for _, library := range next.Libraries {
				available[library.ID] = true
			}
			user.LibraryIDs = make([]int64, 0, len(in.LibraryIDs))
			for _, id := range in.LibraryIDs {
				if !available[id] {
					return errors.New("书库不存在或重复，请刷新后重试")
				}
				delete(available, id)
				user.LibraryIDs = append(user.LibraryIDs, id)
			}
			next.putUser(in.ID, user)
			return nil
		}) {
			reply(w, true)
		}
	}
}
