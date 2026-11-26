package main

// 本文件实现兼容 Casdoor 等身份提供方的 OIDC 登录和管理配置。
import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const oidcTimeout = 12 * time.Second

// OIDCConfig 保存身份提供方、OAuth 客户端和管理员 claim 映射。
type OIDCConfig struct {
	Enabled      bool
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	Scopes       string
	AdminClaim   string
	AdminValue   string
}

type oidcSettings struct {
	OIDCConfig
	HasClientSecret   bool
	ClearClientSecret bool
}

type oidcFlow struct {
	config   OIDCConfig
	nonce    string
	verifier string
	expires  time.Time
}

// oidcProvider 通过标准发现文档读取端点，并配置 ID Token 验证器。
func oidcProvider(ctx context.Context, config OIDCConfig) (*oidc.Provider, *oauth2.Config, *oidc.IDTokenVerifier, error) {
	ctx, cancel := context.WithTimeout(ctx, oidcTimeout)
	defer cancel()
	provider, err := oidc.NewProvider(ctx, config.Issuer)
	if err != nil {
		return nil, nil, nil, errors.New("无法发现 OIDC 身份提供方")
	}
	scopes := strings.Fields(config.Scopes)
	if len(scopes) == 0 {
		scopes = []string{oidc.ScopeOpenID, "profile", "email"}
	}
	if !slices.Contains(scopes, oidc.ScopeOpenID) {
		scopes = append(scopes, oidc.ScopeOpenID)
	}
	oauthConfig := &oauth2.Config{
		ClientID: config.ClientID, ClientSecret: config.ClientSecret, RedirectURL: config.RedirectURL,
		Endpoint: provider.Endpoint(), Scopes: scopes,
	}
	verifier := provider.Verifier(&oidc.Config{ClientID: config.ClientID})
	return provider, oauthConfig, verifier, nil
}

func (config OIDCConfig) validate() error {
	if !config.Enabled {
		return nil
	}
	issuer, err := url.Parse(config.Issuer)
	if err != nil || !validOIDCURL(issuer) || issuer.RawQuery != "" || issuer.Fragment != "" {
		return errors.New("Issuer 必须是有效的 HTTPS URL")
	}
	redirect, err := url.Parse(config.RedirectURL)
	if err != nil || !validOIDCURL(redirect) || redirect.Path != "/api/oidc/callback" || redirect.RawQuery != "" || redirect.Fragment != "" {
		return errors.New("回调地址必须指向 /api/oidc/callback")
	}
	if config.ClientID == "" || config.ClientSecret == "" {
		return errors.New("请填写 OIDC Client ID 和 Client Secret")
	}
	if (config.AdminClaim == "") != (config.AdminValue == "") {
		return errors.New("管理员 Claim 和值必须同时填写")
	}
	return nil
}

func validOIDCURL(value *url.URL) bool {
	return value != nil && value.Hostname() != "" && value.User == nil &&
		(value.Scheme == "https" || (value.Scheme == "http" && isLoopbackHost(value.Hostname())))
}

func isLoopbackHost(host string) bool {
	return strings.EqualFold(host, "localhost") || host == "127.0.0.1" || host == "::1"
}

func (s *Server) oidcLogin(w http.ResponseWriter, r *http.Request) {
	config := s.state.OIDC
	if !config.Enabled {
		fail(w, http.StatusNotFound, "OIDC 登录未启用")
		return
	}
	s.mu.Unlock()
	_, oauthConfig, _, err := oidcProvider(r.Context(), config)
	s.mu.Lock()
	if err != nil {
		fail(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	if config != s.state.OIDC {
		fail(w, http.StatusServiceUnavailable, "OIDC 配置已更新，请重新登录")
		return
	}
	state, nonce, verifier := token(), token(), oauth2.GenerateVerifier()
	if s.oidcFlows == nil {
		s.oidcFlows = make(map[string]oidcFlow)
	}
	now := time.Now()
	for key, flow := range s.oidcFlows {
		if !now.Before(flow.expires) {
			delete(s.oidcFlows, key)
		}
	}
	if len(s.oidcFlows) >= 1024 {
		fail(w, http.StatusTooManyRequests, "OIDC 登录请求过多，请稍后重试")
		return
	}
	s.oidcFlows[state] = oidcFlow{config: config, nonce: nonce, verifier: verifier, expires: now.Add(5 * time.Minute)}
	http.SetCookie(w, &http.Cookie{
		Name: "wacky_oidc_state", Value: state, Path: "/api/oidc/callback", HttpOnly: true,
		Secure: strings.HasPrefix(config.RedirectURL, "https://"), SameSite: http.SameSiteLaxMode, MaxAge: 300,
	})
	http.Redirect(w, r, oauthConfig.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), http.StatusFound)
}

func (s *Server) oidcCallback(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	stateCookie, err := r.Cookie("wacky_oidc_state")
	if err != nil || state == "" || stateCookie.Value != state {
		fail(w, http.StatusBadRequest, "OIDC 登录状态无效，请重新登录")
		return
	}
	callback, _ := url.Parse(s.state.OIDC.RedirectURL)
	if callback == nil || !strings.EqualFold(r.Host, callback.Host) {
		fail(w, http.StatusBadRequest, "OIDC 回调主机与配置不匹配")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: "wacky_oidc_state", Value: "", Path: "/api/oidc/callback", HttpOnly: true,
		Secure: strings.HasPrefix(s.state.OIDC.RedirectURL, "https://"), SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
	flow, exists := s.oidcFlows[state]
	delete(s.oidcFlows, state)
	if !exists || !time.Now().Before(flow.expires) || flow.config != s.state.OIDC {
		fail(w, http.StatusBadRequest, "OIDC 登录请求已过期，请重新登录")
		return
	}
	if r.URL.Query().Get("error") != "" {
		fail(w, http.StatusUnauthorized, "OIDC 身份提供方拒绝登录")
		return
	}
	var idToken *oidc.IDToken
	var claims map[string]any
	err = func() error {
		s.mu.Unlock()
		defer s.mu.Lock()
		_, oauthConfig, verifier, err := oidcProvider(r.Context(), flow.config)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(r.Context(), oidcTimeout)
		defer cancel()
		accessToken, err := oauthConfig.Exchange(ctx, r.URL.Query().Get("code"), oauth2.VerifierOption(flow.verifier))
		if err != nil {
			return errors.New("OIDC 授权码验证失败")
		}
		rawIDToken, ok := accessToken.Extra("id_token").(string)
		if !ok {
			return errors.New("OIDC 响应缺少 ID Token")
		}
		idToken, err = verifier.Verify(ctx, rawIDToken)
		if err != nil || idToken.Nonce != flow.nonce || idToken.Subject == "" {
			return errors.New("OIDC ID Token 验证失败")
		}
		return idToken.Claims(&claims)
	}()
	if err != nil {
		fail(w, http.StatusUnauthorized, err.Error())
		return
	}
	if flow.config != s.state.OIDC {
		fail(w, http.StatusServiceUnavailable, "OIDC 配置已更新，请重新登录")
		return
	}
	readerID := oidcReaderID(flow.config.Issuer, idToken.Subject)
	username := oidcUsername(claims, idToken.Subject)
	user, exists := s.state.Users[readerID]
	if exists && user.Disabled {
		fail(w, http.StatusForbidden, "账户已停用")
		return
	}
	if !exists || user.Username != username || user.Source != "oidc" {
		user.Username, user.Source = username, "oidc"
		if !s.update(w, func(next *State) error {
			next.putUser(readerID, user)
			return nil
		}) {
			return
		}
	}
	session := Session{
		Username: username, Source: "oidc", readerID: readerID,
		Admin:   oidcAdminClaim(claims, flow.config.AdminClaim, flow.config.AdminValue),
		Expires: time.Now().Add(24 * time.Hour),
	}
	for key, other := range s.sessions {
		if !time.Now().Before(other.Expires) {
			delete(s.sessions, key)
		}
	}
	sessionID := token()
	s.sessions[sessionID] = session
	cookie(w, r, sessionID, 86400, strings.HasPrefix(flow.config.RedirectURL, "https://"))
	http.Redirect(w, r, "/#home", http.StatusFound)
}

func oidcReaderID(issuer, subject string) string {
	sum := sha256.Sum256([]byte("oidc\x00" + issuer + "\x00" + subject))
	return "oidc:" + hex.EncodeToString(sum[:])
}

func oidcUsername(claims map[string]any, subject string) string {
	for _, key := range []string{"preferred_username", "email", "name"} {
		if value, ok := claims[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return subject
}

func oidcAdminClaim(claims map[string]any, key, expected string) bool {
	if key == "" || expected == "" {
		return false
	}
	switch value := claims[key].(type) {
	case string:
		return value == expected
	case []any:
		for _, item := range value {
			if text, ok := item.(string); ok && text == expected {
				return true
			}
		}
	case []string:
		return slices.Contains(value, expected)
	}
	return false
}

func (s *Server) oidcSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method + " " + r.URL.Path {
	case "GET /api/oidc":
		out := oidcSettings{OIDCConfig: s.state.OIDC, HasClientSecret: s.state.OIDC.ClientSecret != ""}
		out.ClientSecret = ""
		reply(w, out)
	case "POST /api/oidc", "POST /api/oidc/test":
		var in oidcSettings
		if !decode(w, r, &in) {
			return
		}
		config := in.OIDCConfig
		config.Issuer = strings.TrimSpace(config.Issuer)
		config.ClientID = strings.TrimSpace(config.ClientID)
		config.RedirectURL = strings.TrimSpace(config.RedirectURL)
		config.AdminClaim = strings.TrimSpace(config.AdminClaim)
		config.AdminValue = strings.TrimSpace(config.AdminValue)
		if in.ClearClientSecret {
			config.ClientSecret = ""
		} else if config.ClientSecret == "" && config.Issuer == s.state.OIDC.Issuer && config.ClientID == s.state.OIDC.ClientID {
			config.ClientSecret = s.state.OIDC.ClientSecret
		}
		if r.URL.Path == "/api/oidc/test" {
			config.Enabled = true
		}
		if err := config.validate(); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		if r.URL.Path == "/api/oidc/test" {
			s.mu.Unlock()
			defer s.mu.Lock()
			if _, _, _, err := oidcProvider(r.Context(), config); err != nil {
				fail(w, http.StatusBadRequest, err.Error())
				return
			}
			reply(w, true)
			return
		}
		before := s.state.OIDC
		if s.update(w, func(next *State) error { next.OIDC = config; return nil }) {
			if before != config {
				for id, session := range s.sessions {
					if session.Source == "oidc" {
						delete(s.sessions, id)
					}
				}
			}
			reply(w, true)
		}
	}
}
