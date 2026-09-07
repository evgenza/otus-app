package resilience

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	retries            = promauto.NewCounterVec(prometheus.CounterOpts{Name: "otus_retry_attempts_total", Help: "Выполненные повторные попытки без первого вызова"}, []string{"dependency", "operation"})
	exhausted          = promauto.NewCounterVec(prometheus.CounterOpts{Name: "otus_retry_exhausted_total", Help: "Операции, исчерпавшие установленный лимит попыток"}, []string{"dependency", "operation"})
	breakerCalls       = promauto.NewCounterVec(prometheus.CounterOpts{Name: "otus_circuit_breaker_calls_total", Help: "Результаты вызовов и отклонения открытым circuit breaker"}, []string{"dependency", "result"})
	breakerTrips       = promauto.NewCounterVec(prometheus.CounterOpts{Name: "otus_circuit_breaker_trips_total", Help: "Открытия circuit breaker после ошибок"}, []string{"dependency"})
	breakerTransitions = promauto.NewCounterVec(prometheus.CounterOpts{Name: "otus_circuit_breaker_transitions_total", Help: "Переходы состояния circuit breaker"}, []string{"dependency", "from", "to"})
	breakerState       = promauto.NewGaugeVec(prometheus.GaugeOpts{Name: "otus_circuit_breaker_state", Help: "Состояние circuit breaker: 0 closed, 1 open, 2 half_open"}, []string{"dependency"})
)

func Retried(dependency, operation string)   { retries.WithLabelValues(dependency, operation).Inc() }
func Exhausted(dependency, operation string) { exhausted.WithLabelValues(dependency, operation).Inc() }
