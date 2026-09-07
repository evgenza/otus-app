// Package messaging содержит агрегат сообщения и событие его создания.
// Домен не зависит от транспорта, баз данных и брокеров.
package messaging

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

var (
	ErrEmptyText           = errors.New("поле text обязательно")
	ErrIdempotencyConflict = errors.New("ключ идемпотентности уже использован для другого текста")
)

type Message struct {
	ID         int64     `json:"id"`
	Text       string    `json:"text"`
	Checksum   string    `json:"checksum"`
	ChecksumOK bool      `json:"checksum_ok"`
	CreatedAt  time.Time `json:"created_at"`
}

func ValidateText(text string) error {
	if strings.TrimSpace(text) == "" {
		return ErrEmptyText
	}
	return nil
}

func Checksum(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// MessageCreated неизменно; ID однозначно определяет событие создания сообщения.
type MessageCreated struct {
	ID        int64     `json:"id"`
	Text      string    `json:"text"`
	Checksum  string    `json:"checksum"`
	CreatedAt time.Time `json:"created_at"`
	Producer  string    `json:"producer,omitempty"`
}

func (m Message) CreatedEvent() MessageCreated {
	return MessageCreated{ID: m.ID, Text: m.Text, Checksum: m.Checksum, CreatedAt: m.CreatedAt, Producer: "otus-app"}
}
