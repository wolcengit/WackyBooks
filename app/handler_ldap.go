package main

// 本文件验证 LDAP 配置、建立安全连接、认证用户并提供管理接口。
import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/go-ldap/ldap/v3"
)

// LDAPConfig 保存目录服务器、搜索范围和绑定账户设置。
type LDAPConfig struct {
	Enabled, PlainLDAP                                             bool
	URL, BindDN, BindPassword, BaseDN, UserFilter, IDAttribute, CA string
}

type ldapSettings struct {
	LDAPConfig
	HasBindPassword   bool
	ClearBindPassword bool
	TestUsername      string `json:",omitempty"`
	TestPassword      string `json:",omitempty"`
}

const ldapTimeout = 8 * time.Second

var ldapAttribute = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9-]*$`)
var errLDAPCredentials = errors.New("LDAP 用户名或密码错误")

// validate 限定 LDAP URL、过滤器和证书配置，避免不安全或歧义的查询。
func (c LDAPConfig) validate() (*url.URL, *tls.Config, error) {
	u, err := url.Parse(c.URL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "ldap" && u.Scheme != "ldaps") ||
		u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, nil, errors.New("请输入 LDAP 服务器地址，使用 ldaps:// 或 ldap://")
	}
	if c.PlainLDAP && u.Scheme != "ldap" {
		return nil, nil, errors.New("明文 LDAP 仅适用于 ldap:// 地址")
	}
	if c.BaseDN == "" {
		return nil, nil, errors.New("请输入用户搜索 Base DN")
	}
	if _, err := ldap.ParseDN(c.BaseDN); err != nil {
		return nil, nil, errors.New("用户搜索 Base DN 格式无效")
	}
	if (c.BindDN == "") != (c.BindPassword == "") {
		return nil, nil, errors.New("请同时填写绑定 DN 和密码，或同时留空以匿名搜索")
	}
	if !strings.Contains(c.UserFilter, "{username}") {
		return nil, nil, errors.New("用户过滤器须包含 {username}")
	}
	if _, err := ldap.CompileFilter(strings.ReplaceAll(c.UserFilter, "{username}", "example")); err != nil {
		return nil, nil, errors.New("用户过滤器格式无效")
	}
	if !ldapAttribute.MatchString(c.IDAttribute) {
		return nil, nil, errors.New("用户标识属性格式无效")
	}
	if c.PlainLDAP {
		return u, nil, nil
	}
	config := &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12}
	if c.CA != "" {
		roots, err := x509.SystemCertPool()
		if err != nil || roots == nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM([]byte(c.CA)) {
			return nil, nil, errors.New("无法读取 CA 证书，请提供 PEM 格式")
		}
		config.RootCAs = roots
	}
	return u, config, nil
}

func (c LDAPConfig) connect(ctx context.Context) (*ldap.Conn, error) {
	u, config, err := c.validate()
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: ldapTimeout}
	port := u.Port()
	if port == "" {
		port = "389"
		if u.Scheme == "ldaps" {
			port = "636"
		}
	}
	address := net.JoinHostPort(u.Hostname(), port)
	raw, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("无法连接 LDAP 服务器: %w", err)
	}
	raw.SetDeadline(time.Now().Add(ldapTimeout))
	var transport net.Conn = raw
	if u.Scheme == "ldaps" {
		encrypted := tls.Client(raw, config)
		if err := encrypted.HandshakeContext(ctx); err != nil {
			raw.Close()
			return nil, fmt.Errorf("LDAP TLS 握手失败: %w", err)
		}
		transport = encrypted
	}
	conn := ldap.NewConn(transport, u.Scheme == "ldaps")
	conn.Start()
	conn.SetTimeout(ldapTimeout)
	if u.Scheme == "ldap" && !c.PlainLDAP {
		if err := conn.StartTLS(config); err != nil {
			conn.Close()
			return nil, fmt.Errorf("LDAP StartTLS 失败: %w", err)
		}
	}
	if c.BindDN != "" {
		if err := conn.Bind(c.BindDN, c.BindPassword); err != nil {
			conn.Close()
			return nil, fmt.Errorf("LDAP 服务账户绑定失败: %w", err)
		}
	}
	return conn, nil
}

// authenticate 搜索唯一用户条目并校验密码，返回稳定的目录用户标识。
func (c LDAPConfig) authenticate(ctx context.Context, username, password string) (string, error) {
	if password == "" {
		return "", errLDAPCredentials
	}
	conn, err := c.connect(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	filter := strings.ReplaceAll(c.UserFilter, "{username}", ldap.EscapeFilter(username))
	useDN := strings.EqualFold(c.IDAttribute, "dn")
	attribute := c.IDAttribute
	if useDN {
		attribute = "1.1"
	}
	result, err := conn.Search(ldap.NewSearchRequest(c.BaseDN, ldap.ScopeWholeSubtree,
		ldap.NeverDerefAliases, 2, int(ldapTimeout/time.Second), false, filter, []string{attribute}, nil))
	if ldap.IsErrorWithCode(err, ldap.LDAPResultSizeLimitExceeded) {
		return "", fmt.Errorf("%w：搜索匹配了多个用户", errLDAPCredentials)
	}
	if err != nil {
		return "", fmt.Errorf("LDAP 用户搜索失败: %w", err)
	}
	if len(result.Entries) == 0 {
		return "", fmt.Errorf("%w：搜索未找到用户", errLDAPCredentials)
	}
	if len(result.Entries) > 1 {
		return "", fmt.Errorf("%w：搜索匹配了多个用户", errLDAPCredentials)
	}
	entry := result.Entries[0]
	if entry.DN == "" {
		return "", errLDAPCredentials
	}
	if err := conn.Bind(entry.DN, password); err != nil {
		if ldap.IsErrorWithCode(err, ldap.LDAPResultInvalidCredentials) {
			return "", fmt.Errorf("%w：用户绑定失败", errLDAPCredentials)
		}
		return "", fmt.Errorf("LDAP 用户绑定失败: %w", err)
	}
	if useDN {
		key := sha256.Sum256([]byte("ldap-dn\x00" + entry.DN))
		return hex.EncodeToString(key[:]), nil
	}
	ids := entry.GetEqualFoldRawAttributeValues(c.IDAttribute)
	if len(ids) != 1 || len(ids[0]) == 0 {
		return "", fmt.Errorf("用户密码验证通过，但无法读取唯一标识属性 %s。请检查属性读取权限，或选择 dn", c.IDAttribute)
	}
	key := sha256.Sum256(ids[0])
	return hex.EncodeToString(key[:]), nil
}

func (s *Server) ldapSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method + " " + r.URL.Path {
	case "GET /api/ldap":
		out := ldapSettings{LDAPConfig: s.state.LDAP, HasBindPassword: s.state.LDAP.BindPassword != ""}
		out.BindPassword = ""
		reply(w, out)
	case "POST /api/ldap", "POST /api/ldap/test":
		var in ldapSettings
		if !decode(w, r, &in) {
			return
		}
		config := in.LDAPConfig
		config.URL = strings.TrimSpace(config.URL)
		config.BaseDN = strings.TrimSpace(config.BaseDN)
		config.BindDN = strings.TrimSpace(config.BindDN)
		config.UserFilter = strings.TrimSpace(config.UserFilter)
		config.IDAttribute = strings.TrimSpace(config.IDAttribute)
		if in.ClearBindPassword {
			config.BindPassword = ""
		} else if config.BindPassword == "" && config.BindDN != "" && config.BindDN == s.state.LDAP.BindDN {
			config.BindPassword = s.state.LDAP.BindPassword
		}
		if r.URL.Path == "/api/ldap/test" {
			s.mu.Unlock()
			defer s.mu.Lock()
			username := strings.TrimSpace(in.TestUsername)
			if username != "" || in.TestPassword != "" {
				if username == "" || in.TestPassword == "" || len(username) > 512 || len(in.TestPassword) > 4096 {
					fail(w, 400, "请填写有效的 LDAP 测试账号和密码")
					return
				}
				if _, err := config.authenticate(r.Context(), username, in.TestPassword); err != nil {
					fail(w, 400, err.Error())
					return
				}
				reply(w, true)
				return
			}
			conn, err := config.connect(r.Context())
			if err == nil {
				defer conn.Close()
				var result *ldap.SearchResult
				result, err = conn.Search(ldap.NewSearchRequest(config.BaseDN, ldap.ScopeBaseObject,
					ldap.NeverDerefAliases, 1, int(ldapTimeout/time.Second), false, "(objectClass=*)", []string{"1.1"}, nil))
				if err != nil {
					err = fmt.Errorf("无法读取用户搜索 Base DN: %w", err)
				} else if len(result.Entries) != 1 {
					err = errors.New("无法读取用户搜索 Base DN，请检查目录权限")
				}
			}
			if err != nil {
				fail(w, 400, err.Error())
				return
			}
			reply(w, true)
			return
		}
		if config.Enabled {
			if _, _, err := config.validate(); err != nil {
				fail(w, 400, err.Error())
				return
			}
		}
		before := s.state.LDAP
		if s.update(w, func(next *State) error { next.LDAP = config; return nil }) {
			if config != before {
				for id, session := range s.sessions {
					if session.Source == "ldap" {
						delete(s.sessions, id)
					}
				}
			}
			reply(w, true)
		}
	}
}
