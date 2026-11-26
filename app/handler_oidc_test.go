package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestOIDCConfigValidationAndAdminClaim(t *testing.T) {
	config := OIDCConfig{
		Enabled: true, Issuer: "https://sso.example.org", ClientID: "books",
		ClientSecret: "secret", RedirectURL: "https://books.example.org/api/oidc/callback",
	}
	if err := config.validate(); err != nil {
		t.Fatal(err)
	}
	config.RedirectURL = "https://books.example.org/callback"
	if err := config.validate(); err == nil {
		t.Fatal("expected invalid callback path to be rejected")
	}
	if !oidcAdminClaim(map[string]any{"groups": []any{"readers", "admins"}}, "groups", "admins") {
		t.Fatal("expected matching group claim to grant the configured role")
	}
	if oidcAdminClaim(map[string]any{"groups": []any{"readers"}}, "groups", "admins") {
		t.Fatal("unexpected administrator role")
	}
}

func TestOIDCAuthorizationCodeCallback(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	const subject = "subject-123"
	var issuer string
	var nonce string
	var challengedVerifier string
	providerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			issuer = "http://" + r.Host
			json.NewEncoder(w).Encode(map[string]any{
				"issuer": issuer, "authorization_endpoint": issuer + "/authorize",
				"token_endpoint": issuer + "/token", "jwks_uri": issuer + "/keys",
			})
		case "/authorize":
			nonce = r.URL.Query().Get("nonce")
			challengedVerifier = r.URL.Query().Get("code_challenge")
			if r.URL.Query().Get("code_challenge_method") != "S256" {
				t.Errorf("expected S256 PKCE challenge")
			}
			w.WriteHeader(http.StatusOK)
		case "/keys":
			json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{
				"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "test-key",
				"n": base64.RawURLEncoding.EncodeToString(privateKey.PublicKey.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1}),
			}}})
		case "/token":
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			verifier := r.Form.Get("code_verifier")
			hash := sha256.Sum256([]byte(verifier))
			if verifier == "" || base64.RawURLEncoding.EncodeToString(hash[:]) != challengedVerifier {
				t.Errorf("invalid PKCE verifier")
			}
			now := time.Now()
			header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"test-key","typ":"JWT"}`))
			claims, _ := json.Marshal(map[string]any{
				"iss": issuer, "sub": subject, "aud": "books", "exp": now.Add(time.Minute).Unix(),
				"iat": now.Unix(), "nonce": nonce, "preferred_username": "reader@example.org",
				"groups": []string{"wackybooks-admin"},
			})
			payload := base64.RawURLEncoding.EncodeToString(claims)
			message := header + "." + payload
			digest := sha256.Sum256([]byte(message))
			signature, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, digest[:])
			if err != nil {
				t.Error(err)
			}
			idToken := message + "." + base64.RawURLEncoding.EncodeToString(signature)
			w.Header().Set("Content-Type", "application/json")
			response := map[string]any{
				"access_token": "access", "token_type": "Bearer", "expires_in": 300, "id_token": idToken,
			}
			json.NewEncoder(w).Encode(response)
		default:
			http.NotFound(w, r)
		}
	}))
	defer providerServer.Close()

	config := OIDCConfig{
		Enabled: true, Issuer: providerServer.URL, ClientID: "books", ClientSecret: "secret",
		RedirectURL: "http://127.0.0.1:18095/api/oidc/callback", AdminClaim: "groups", AdminValue: "wackybooks-admin",
	}
	db, err := openStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	state, err := loadSettings(db)
	if err != nil {
		t.Fatal(err)
	}
	state.OIDC = config
	s := &Server{db: db, state: state, sessions: map[string]Session{}, oidcFlows: map[string]oidcFlow{}}

	login := httptest.NewRequest(http.MethodGet, "/api/oidc/login", nil)
	loginRecorder := httptest.NewRecorder()
	s.mu.Lock()
	s.oidcLogin(loginRecorder, login)
	s.mu.Unlock()
	if loginRecorder.Code != http.StatusFound {
		t.Fatalf("OIDC login returned %d", loginRecorder.Code)
	}
	authorizationURL, err := url.Parse(loginRecorder.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	flowState := authorizationURL.Query().Get("state")
	nonce = authorizationURL.Query().Get("nonce")
	challengedVerifier = authorizationURL.Query().Get("code_challenge")
	if flowState == "" || nonce == "" {
		t.Fatal("authorization request is missing state or nonce")
	}
	cookie := loginRecorder.Result().Cookies()[0]

	callback := httptest.NewRequest(http.MethodGet, config.RedirectURL+"?state="+url.QueryEscape(flowState)+"&code=valid-code", nil)
	callback.AddCookie(cookie)
	callbackRecorder := httptest.NewRecorder()
	s.mu.Lock()
	s.oidcCallback(callbackRecorder, callback)
	s.mu.Unlock()
	if callbackRecorder.Code != http.StatusFound || callbackRecorder.Header().Get("Location") != "/#home" {
		t.Fatalf("OIDC callback returned %d, body %s", callbackRecorder.Code, callbackRecorder.Body.String())
	}
	readerID := oidcReaderID(config.Issuer, subject)
	user, exists := s.state.Users[readerID]
	if !exists || user.Source != "oidc" || user.Username != "reader@example.org" {
		t.Fatalf("OIDC user was not registered correctly: %+v", user)
	}
	sessionCookie := callbackRecorder.Result().Cookies()
	if len(sessionCookie) < 2 {
		t.Fatalf("expected state and session cookies, got %d", len(sessionCookie))
	}
	var session Session
	found := false
	for _, value := range s.sessions {
		if value.readerID == readerID {
			session, found = value, true
		}
	}
	if !found || !session.Admin || session.Source != "oidc" {
		t.Fatalf("OIDC session was not assigned expected claims: %+v", session)
	}
}

func TestOIDCReaderIDSeparatesIssuers(t *testing.T) {
	first := oidcReaderID("https://first.example.org", "subject")
	second := oidcReaderID("https://second.example.org", "subject")
	if first == second || !strings.HasPrefix(first, "oidc:") {
		t.Fatalf("unexpected OIDC reader IDs: %q, %q", first, second)
	}
}
