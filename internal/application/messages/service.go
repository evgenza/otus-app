// Package messages задает порты команд и запросов контекста сообщений.
package messages

import (
	"context"
	"time"

	"github.com/evgenza/otus-app/internal/domain/messaging"
)

type Commands interface {
	// Create атомарно сохраняет сообщение и задания на доставку события.
	Create(context.Context, string, string) (messaging.Message, error)
}

type Queries interface {
	List(context.Context) ([]messaging.Message, error)
}

type Store interface {
	Commands
	Queries
}

// Service обеспечивает одинаковые инварианты и таймауты для HTTP и gRPC.
type Service struct {
	commands Commands
	queries  Queries
}

func New(commands Commands, queries Queries) *Service {
	return &Service{commands: commands, queries: queries}
}

func (s *Service) Create(ctx context.Context, text, key string) (messaging.Message, error) {
	if err := messaging.ValidateText(text); err != nil {
		return messaging.Message{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return s.commands.Create(ctx, text, key)
}

func (s *Service) List(ctx context.Context) ([]messaging.Message, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return s.queries.List(ctx)
}
