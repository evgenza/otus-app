package broker

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
)

type stubReader struct {
	messages  []kafka.Message
	fetched   int
	committed []int64
	commitErr error
}

func TestKafkaMetadataHonorsDeadline(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lis.Close() }()
	done := make(chan struct{})
	defer close(done)
	go func() {
		conn, err := lis.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		// TCP доступен, но ответ с метаданными Kafka не приходит.
		<-done
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	err = ensureTopic(ctx, []string{lis.Addr().String()}, "test", 1, 1)
	if err == nil {
		t.Fatal("молчащий брокер не должен считаться доступным")
	}
	if time.Since(started) > time.Second {
		t.Fatal("запрос метаданных проигнорировал дедлайн")
	}
}

func (r *stubReader) FetchMessage(ctx context.Context) (kafka.Message, error) {
	if r.fetched == len(r.messages) {
		<-ctx.Done()
		return kafka.Message{}, ctx.Err()
	}
	m := r.messages[r.fetched]
	r.fetched++
	return m, nil
}
func (r *stubReader) CommitMessages(_ context.Context, messages ...kafka.Message) error {
	for _, m := range messages {
		r.committed = append(r.committed, m.Offset)
	}
	return r.commitErr
}
func (*stubReader) Close() error { return nil }

func TestKafkaDoesNotSkipFailedRecord(t *testing.T) {
	for _, scenario := range []string{"обработка", "декодирование", "подтверждение"} {
		t.Run(scenario, func(t *testing.T) {
			raw := []byte(`{"id":1,"text":"сообщение"}`)
			if scenario == "декодирование" {
				raw = []byte(`сломанный JSON`)
			}
			r := &stubReader{messages: []kafka.Message{{Partition: 0, Offset: 10, Value: raw}, {Partition: 0, Offset: 11, Value: []byte(`{"id":2}`)}}}
			if scenario == "подтверждение" {
				r.commitErr = errors.New("нет кворума")
			}
			c := &kafkaConsumer{reader: r}
			err := c.Consume(context.Background(), func(context.Context, Event) error {
				if scenario == "обработка" {
					return errors.New("хранилище недоступно")
				}
				return nil
			})
			if err == nil {
				t.Fatal("ошибка должна завершить читателя для безопасного перезапуска")
			}
			if r.fetched != 1 {
				t.Fatalf("после ошибки прочитано следующее событие: %d", r.fetched)
			}
			if scenario != "подтверждение" && len(r.committed) != 0 {
				t.Fatal("необработанное событие подтверждено")
			}
		})
	}
}
