package resilience

import (
	"errors"
	"io"
	"net"
	"net/http"
	"time"
)

var errTemporaryHTTP = errors.New("зависимый HTTP-сервис временно недоступен")

type HTTPTransport struct {
	base       http.RoundTripper
	breaker    *Breaker
	policy     Policy
	dependency string
}

func HTTPBase() *http.Transport {
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.DialContext = (&net.Dialer{Timeout: time.Second, KeepAlive: 30 * time.Second}).DialContext
	base.ResponseHeaderTimeout = 3 * time.Second
	return base
}

func NewHTTPTransport(dependency string, base http.RoundTripper) *HTTPTransport {
	if base == nil {
		base = HTTPBase()
	}
	return &HTTPTransport{base: base, breaker: NewBreaker(dependency), dependency: dependency,
		policy: Policy{Attempts: 3, Base: 100 * time.Millisecond, Max: 2 * time.Second}}
}

func (t *HTTPTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// PUT перезаписывает ресурс по стабильному ID. POST без такого контракта не повторяем.
	safe := req.Method == http.MethodGet || req.Method == http.MethodHead || req.Method == http.MethodPut
	safe = safe && (req.Body == nil || req.Body == http.NoBody || req.GetBody != nil)
	var response *http.Response
	attempt := 0
	err := t.breaker.Do(req.Context(), func() error {
		return t.policy.Do(req.Context(), t.dependency, "http", func(err error) bool {
			var netErr net.Error
			return safe && (errors.Is(err, errTemporaryHTTP) || errors.As(err, &netErr) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF))
		}, func() error {
			next := req.Clone(req.Context())
			if attempt > 0 {
				if response != nil {
					// Не ждем тело ошибочного ответа: сервис может остановиться после заголовков.
					_ = response.Body.Close()
				}
				if req.GetBody != nil {
					body, err := req.GetBody()
					if err != nil {
						return err
					}
					next.Body = body
				}
			}
			attempt++
			var err error
			response, err = t.base.RoundTrip(next)
			if err != nil {
				return err
			}
			switch response.StatusCode {
			case 429, 500, 502, 503, 504:
				return errTemporaryHTTP
			default:
				return nil
			}
		})
	})
	if errors.Is(err, errTemporaryHTTP) {
		return response, nil
	}
	if err != nil {
		if response != nil {
			_ = response.Body.Close()
		}
		if attempt == 0 && req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	return response, nil
}
