package broker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakePublisher struct {
	name string
	fail bool

	mu   sync.Mutex
	sent []Event
}

func (f *fakePublisher) Name() string { return f.name }

func (f *fakePublisher) Publish(_ context.Context, ev Event) error {
	if f.fail {
		return errors.New("брокер недоступен")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, ev)
	return nil
}

func (f *fakePublisher) Close() error { return nil }

func TestBusPublishesToAllBrokers(t *testing.T) {
	first := &fakePublisher{name: "kafka"}
	second := &fakePublisher{name: "nats"}
	bus := &Bus{publishers: []Publisher{first, second}}

	bus.Publish(context.Background(), Event{ID: 1, Text: "привет"})

	for _, p := range []*fakePublisher{first, second} {
		if len(p.sent) != 1 || p.sent[0].ID != 1 {
			t.Fatalf("брокер %s не получил событие: %v", p.name, p.sent)
		}
	}
}

// Отказ одного брокера не должен мешать остальным: публикация best-effort.
func TestBusSurvivesBrokenPublisher(t *testing.T) {
	broken := &fakePublisher{name: "rabbitmq", fail: true}
	alive := &fakePublisher{name: "kafka"}
	bus := &Bus{publishers: []Publisher{broken, alive}}

	bus.Publish(context.Background(), Event{ID: 2, Text: "событие"})

	if len(alive.sent) != 1 {
		t.Fatalf("живой брокер должен был получить событие, получил %d", len(alive.sent))
	}
	names := bus.Names()
	if len(names) != 2 || names[0] != "rabbitmq" || names[1] != "kafka" {
		t.Fatalf("неверный список брокеров: %v", names)
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
