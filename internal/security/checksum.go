package security

import "github.com/evgenza/otus-app/internal/domain/messaging"

func Checksum(text string) string { return messaging.Checksum(text) }
