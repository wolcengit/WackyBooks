package main

// 本文件处理本地账户、LDAP 账户的登录及登录名解析。
import (
	"errors"
	"log"
	"net/http"
	"strings"
	"time"
)

// usesLDAPLogin 将含 @ 的登录名路由到 LDAP，其余登录名只查本地账户。
func usesLDAPLogin(username string) bool { return strings.Contains(username, "@") }

// login 验证账户凭据并创建带安全属性的会话 Cookie。
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var in struct{ Username, Password string }
	if !decode(w, r, &in) {
		return
	}
	in.Username = strings.TrimSpace(in.Username)
	if in.Username == "" {
		in.Username = "Admin"
	}
	directoryLogin := usesLDAPLogin(in.Username)
	var localID string
	var local User
	if !directoryLogin {
		localID, local = s.state.localUser(in.Username)
	}
	session := Session{Username: in.Username, Admin: !directoryLogin && strings.EqualFold(in.Username, "Admin")}
	if session.Admin {
		session.Username = "Admin"
		session.Source = "local"
		if len(in.Password) > 72 {
			fail(w, 400, "密码不能超过 72 字节")
			return
		}
		if s.state.Password == "" {
			if len(in.Password) < 8 {
				fail(w, 400, "密码至少需要 8 字节")
				return
			}
			if !s.update(w, func(next *State) error { next.Password = passwordHash(in.Password, token()); return nil }) {
				return
			}
		} else if !checkPassword(in.Password, s.state.Password) {
			fail(w, 401, "用户名或密码错误")
			return
		}
	} else if localID != "" {
		if local.Disabled || len(in.Password) > 72 || !checkPassword(in.Password, local.PasswordHash) {
			fail(w, 401, "用户名或密码错误")
			return
		}
		session.Username, session.Source, session.readerID = local.Username, local.Source, localID
	} else if directoryLogin {
		config := s.state.LDAP
		if !config.Enabled || len(in.Username) > 512 || len(in.Password) > 4096 {
			fail(w, 401, "用户名或密码错误")
			return
		}
		s.mu.Unlock()
		readerID, err := config.authenticate(r.Context(), in.Username, in.Password)
		s.mu.Lock()
		if config != s.state.LDAP {
			fail(w, 503, "LDAP 配置已更新，请重新登录")
			return
		}
		if errors.Is(err, errLDAPCredentials) {
			fail(w, 401, "用户名或密码错误")
			return
		}
		if err != nil {
			log.Printf("LDAP authentication failed: %v", err)
			fail(w, 503, "LDAP 登录暂不可用")
			return
		}
		if !s.registerLDAP(w, readerID, in.Username) {
			return
		}
		session.readerID, session.Source = readerID, "ldap"
	} else {
		fail(w, 401, "用户名或密码错误")
		return
	}
	now := time.Now()
	for id, other := range s.sessions {
		if !now.Before(other.Expires) {
			delete(s.sessions, id)
		}
	}
	if previous, err := r.Cookie("wacky_session"); err == nil {
		delete(s.sessions, previous.Value)
	}
	id := token()
	session.Expires = now.Add(24 * time.Hour)
	s.sessions[id] = session
	cookie(w, r, id, 86400, false)
	reply(w, session)
}
