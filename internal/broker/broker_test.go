package broker

import (
	"context"
	"testing"
	"time"
)

func TestConfiguredNamesIncludesUnavailableDestinations(t *testing.T) {
	t.Setenv("KAFKA_BROKERS", "unavailable:9092")
	t.Setenv("RABBITMQ_URLS", "amqp://unavailable:5672")
	t.Setenv("NATS_URLS", "nats://unavailable:4222")
	names := ConfiguredNames()
	if len(names) != 3 {
		t.Fatalf("потеряны настроенные брокеры: %v", names)
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	original := Event{
		ID:        42,
		Text:      "сообщение с русским текстом",
		Checksum:  "9f86d081884c7d65",
		CreatedAt: time.Now().UTC().Truncate(time.Millisecond),
		Producer:  "otus-app",
	}
	raw, err := encode(original)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if decoded != original {
		t.Fatalf("событие исказилось при сериализации:\nбыло:  %+v\nстало: %+v", original, decoded)
	}
}

func TestNewConsumerRejectsUnknownBroker(t *testing.T) {
	if _, err := NewConsumer(context.Background(), "activemq"); err == nil {
		t.Fatal("неизвестный брокер должен приводить к ошибке")
	}
}

func TestEnvListSkipsEmptyItems(t *testing.T) {
	t.Setenv("TEST_BROKER_LIST", " a:9092 , ,b:9092 ")
	got := envList("TEST_BROKER_LIST")
	if len(got) != 2 || got[0] != "a:9092" || got[1] != "b:9092" {
		t.Fatalf("список адресов разобран неверно: %v", got)
	}
	t.Setenv("TEST_BROKER_LIST", "")
	if envList("TEST_BROKER_LIST") != nil {
		t.Fatal("пустая переменная должна давать nil")
	}
}
