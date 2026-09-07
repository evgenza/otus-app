package handlers

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func TestReadinessDoesNotChangeLiveness(t *testing.T) {
	h := New(&fakeStore{}, nil, WithReadiness(func(ctx context.Context) error {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("у проверки готовности нет таймаута")
		}
		return errors.New("база данных недоступна")
	}))
	if rec := request(t, h, http.MethodGet, "/ready", "", nil); rec.Code != 503 {
		t.Fatalf("готовность при отказе БД: %d", rec.Code)
	}
	if rec := request(t, h, http.MethodGet, "/health", "", nil); rec.Code != 200 {
		t.Fatalf("живость при отказе БД: %d", rec.Code)
	}
}
