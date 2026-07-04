package security

import (
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type Auth struct {
	jwksURL string
	issuer  string
	client  *http.Client

	mu          sync.Mutex
	keys        map[string]*rsa.PublicKey
	lastRefresh time.Time
}

func NewAuth() *Auth {
	jwksURL := os.Getenv("AUTH_JWKS_URL")
	if jwksURL == "" {
		return nil
	}
	return &Auth{
		jwksURL: jwksURL,
		issuer:  os.Getenv("AUTH_ISSUER"),
		client:  &http.Client{Timeout: 5 * time.Second},
	}
}

func (a *Auth) Middleware(next http.Handler) http.Handler {
	if a == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		raw, found := strings.CutPrefix(header, "Bearer ")
		if !found || raw == "" {
			writeAuthError(w, "требуется токен авторизации")
			return
		}

		opts := []jwt.ParserOption{
			jwt.WithValidMethods([]string{"RS256"}),
			jwt.WithExpirationRequired(),
		}
		if a.issuer != "" {
			opts = append(opts, jwt.WithIssuer(a.issuer))
		}
		if _, err := jwt.Parse(raw, a.keyFor, opts...); err != nil {
			writeAuthError(w, "недействительный токен")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *Auth) keyFor(token *jwt.Token) (any, error) {
	kid, _ := token.Header["kid"].(string)
	if kid == "" {
		return nil, errors.New("в токене нет kid")
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if key, ok := a.keys[kid]; ok {
		return key, nil
	}
	if time.Since(a.lastRefresh) < 30*time.Second {
		return nil, errors.New("ключ подписи не найден")
	}
	if err := a.refreshKeys(); err != nil {
		return nil, err
	}
	if key, ok := a.keys[kid]; ok {
		return key, nil
	}
	return nil, errors.New("ключ подписи не найден")
}

func (a *Auth) refreshKeys() error {
	resp, err := a.client.Get(a.jwksURL)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return errors.New("JWKS вернул статус " + resp.Status)
	}

	var doc struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return err
	}

	keys := make(map[string]*rsa.PublicKey)
	for _, k := range doc.Keys {
		if k.Kty != "RSA" {
			continue
		}
		n, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			continue
		}
		e, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{
			N: new(big.Int).SetBytes(n),
			E: int(new(big.Int).SetBytes(e).Int64()),
		}
	}
	if len(keys) == 0 {
		return errors.New("в JWKS нет RSA-ключей")
	}
	a.keys = keys
	a.lastRefresh = time.Now()
	return nil
}

func writeAuthError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
