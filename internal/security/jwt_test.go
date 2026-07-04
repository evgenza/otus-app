package security

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func newTestAuth(t *testing.T) (*Auth, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("не удалось сгенерировать ключ: %v", err)
	}

	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]string{{
				"kty": "RSA",
				"kid": "test-key",
				"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
			}},
		})
	}))
	t.Cleanup(jwks.Close)

	a := &Auth{
		jwksURL: jwks.URL,
		issuer:  "test-issuer",
		client:  jwks.Client(),
	}
	return a, key
}

func signToken(t *testing.T, key *rsa.PrivateKey, claims jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = "test-key"
	raw, err := token.SignedString(key)
	if err != nil {
		t.Fatalf("не удалось подписать токен: %v", err)
	}
	return raw
}

func protectedRequest(a *Auth, token string) *httptest.ResponseRecorder {
	handler := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/messages", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestJWTValidToken(t *testing.T) {
	a, key := newTestAuth(t)
	token := signToken(t, key, jwt.MapClaims{
		"iss": "test-issuer",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	if rec := protectedRequest(a, token); rec.Code != http.StatusOK {
		t.Errorf("ожидался статус 200, получен %d", rec.Code)
	}
}

func TestJWTMissingToken(t *testing.T) {
	a, _ := newTestAuth(t)
	rec := protectedRequest(a, "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("ожидался статус 401, получен %d", rec.Code)
	}
}

func TestJWTExpiredToken(t *testing.T) {
	a, key := newTestAuth(t)
	token := signToken(t, key, jwt.MapClaims{
		"iss": "test-issuer",
		"exp": time.Now().Add(-time.Hour).Unix(),
	})
	if rec := protectedRequest(a, token); rec.Code != http.StatusUnauthorized {
		t.Errorf("ожидался статус 401, получен %d", rec.Code)
	}
}

func TestJWTWrongIssuer(t *testing.T) {
	a, key := newTestAuth(t)
	token := signToken(t, key, jwt.MapClaims{
		"iss": "другой-издатель",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	if rec := protectedRequest(a, token); rec.Code != http.StatusUnauthorized {
		t.Errorf("ожидался статус 401, получен %d", rec.Code)
	}
}

func TestJWTForeignKey(t *testing.T) {
	a, _ := newTestAuth(t)
	foreign, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("не удалось сгенерировать ключ: %v", err)
	}
	token := signToken(t, foreign, jwt.MapClaims{
		"iss": "test-issuer",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	if rec := protectedRequest(a, token); rec.Code != http.StatusUnauthorized {
		t.Errorf("ожидался статус 401, получен %d", rec.Code)
	}
}

func TestNilAuthPassesThrough(t *testing.T) {
	var a *Auth
	handler := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/messages", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("nil-auth должен пропускать запросы, получен статус %d", rec.Code)
	}
}
