package resilience

import (
	"context"
	"errors"
	"sync"
	"time"
)

var ErrOpen = errors.New("circuit breaker открыт, зависимый сервис временно недоступен")

type state int

const (
	closed state = iota
	open
	halfOpen
)

func (s state) String() string { return [...]string{"closed", "open", "half_open"}[s] }

type Breaker struct {
	mu         sync.Mutex
	dependency string
	state      state
	failures   int
	generation uint64
	probe      bool
	until      time.Time
	now        func() time.Time
}

func NewBreaker(dependency string) *Breaker {
	breakerState.WithLabelValues(dependency).Set(0)
	return &Breaker{dependency: dependency, now: time.Now}
}

// Do открывает цепь после трех ошибок подряд. Через 5-10 секунд разрешен один пробный вызов.
func (b *Breaker) Do(ctx context.Context, call func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	generation, allowed := b.admit()
	if !allowed {
		breakerCalls.WithLabelValues(b.dependency, "rejected").Inc()
		return ErrOpen
	}
	err := call()
	result := "success"
	if errors.Is(err, context.Canceled) {
		result = "canceled"
	} else if err != nil {
		result = "error"
	}
	breakerCalls.WithLabelValues(b.dependency, result).Inc()
	b.complete(generation, err)
	return err
}

func (b *Breaker) admit() (uint64, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == open {
		if b.now().Before(b.until) {
			return 0, false
		}
		b.transition(halfOpen)
	}
	if b.state == halfOpen {
		if b.probe {
			return 0, false
		}
		b.probe = true
	}
	return b.generation, true
}

func (b *Breaker) complete(generation uint64, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	// Ответ старого вызова не может закрыть цепь после ее открытия.
	if generation != b.generation {
		return
	}
	b.probe = false
	if errors.Is(err, context.Canceled) {
		return
	}
	if err == nil {
		b.failures = 0
		if b.state == halfOpen {
			b.transition(closed)
		}
		return
	}
	b.failures++
	if b.state == halfOpen || b.failures >= 3 {
		b.until = b.now().Add(Jitter(1, 10*time.Second, 10*time.Second))
		b.transition(open)
		breakerTrips.WithLabelValues(b.dependency).Inc()
	}
}

func (b *Breaker) transition(next state) {
	breakerTransitions.WithLabelValues(b.dependency, b.state.String(), next.String()).Inc()
	b.state = next
	b.generation++
	breakerState.WithLabelValues(b.dependency).Set(float64(next))
}
