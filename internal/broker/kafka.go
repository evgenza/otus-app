package broker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/segmentio/kafka-go"
)

type kafkaPublisher struct {
	writer *kafka.Writer
}

func kafkaSettings() (brokers []string, topic, group string, partitions, replicas int) {
	brokers = envList("KAFKA_BROKERS")
	topic = env("KAFKA_TOPIC", "otus.messages")
	group = env("KAFKA_GROUP", "otus-consumer")
	partitions, _ = strconv.Atoi(env("KAFKA_PARTITIONS", "6"))
	replicas, _ = strconv.Atoi(env("KAFKA_REPLICATION", "3"))
	return
}

func newKafkaPublisher(ctx context.Context) (Publisher, error) {
	brokers, topic, _, partitions, replicas := kafkaSettings()
	if len(brokers) == 0 {
		return nil, nil
	}
	if err := ensureTopic(ctx, brokers, topic, partitions, replicas); err != nil {
		return nil, err
	}
	writer := &kafka.Writer{
		Addr:  kafka.TCP(brokers...),
		Topic: topic,
		// Ключ события - id сообщения, поэтому события одного сообщения
		// всегда попадают в одну партицию и не переупорядочиваются.
		Balancer: &kafka.Hash{},
		// acks=all: продюсер ждет подтверждения от всех синхронных реплик,
		// иначе отказ лидера партиции терял бы уже принятые события.
		RequiredAcks: kafka.RequireAll,
		Async:        false,
		BatchTimeout: 10 * time.Millisecond,
		WriteTimeout: 5 * time.Second,
		MaxAttempts:  5,
	}
	return &kafkaPublisher{writer: writer}, nil
}

// ensureTopic создает топик с нужным числом партиций и репликацией:
// автосоздание топика брокером дало бы одну реплику, и отказ узла терял бы
// данные.
func ensureTopic(ctx context.Context, brokers []string, topic string, partitions, replicas int) error {
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	deadline, _ := dialCtx.Deadline()

	var lastErr error
	for _, addr := range brokers {
		conn, err := (&kafka.Dialer{Timeout: 5 * time.Second}).DialContext(dialCtx, "tcp", addr)
		if err != nil {
			lastErr = err
			continue
		}
		// DialContext ограничивает только подключение. Метаданные тоже
		// должны иметь дедлайн, иначе молчащий брокер заблокирует воркер.
		_ = conn.SetDeadline(deadline)
		controller, err := conn.Controller()
		_ = conn.Close()
		if err != nil {
			lastErr = err
			continue
		}
		ctrlAddr := controller.Host + ":" + strconv.Itoa(controller.Port)
		ctrlConn, err := (&kafka.Dialer{Timeout: 5 * time.Second}).DialContext(dialCtx, "tcp", ctrlAddr)
		if err != nil {
			lastErr = err
			continue
		}
		_ = ctrlConn.SetDeadline(deadline)
		err = ctrlConn.CreateTopics(kafka.TopicConfig{
			Topic:             topic,
			NumPartitions:     partitions,
			ReplicationFactor: replicas,
			ConfigEntries: []kafka.ConfigEntry{
				// Запись подтверждается, пока живы хотя бы две реплики.
				{ConfigName: "min.insync.replicas", ConfigValue: "2"},
			},
		})
		_ = ctrlConn.Close()
		if err != nil {
			lastErr = err
			continue
		}
		slog.Info("топик Kafka готов", "topic", topic, "partitions", partitions, "replication", replicas)
		return nil
	}
	return lastErr
}

func (k *kafkaPublisher) Name() string { return "kafka" }

func (k *kafkaPublisher) Publish(ctx context.Context, ev Event) error {
	payload, err := encode(ev)
	if err != nil {
		return err
	}
	return k.writer.WriteMessages(ctx, kafka.Message{
		Key:   []byte(strconv.FormatInt(ev.ID, 10)),
		Value: payload,
		Time:  time.Now(),
	})
}

func (k *kafkaPublisher) Close() error { return k.writer.Close() }

type kafkaReader interface {
	FetchMessage(context.Context) (kafka.Message, error)
	CommitMessages(context.Context, ...kafka.Message) error
	Close() error
}

type kafkaConsumer struct {
	reader kafkaReader
}

func newKafkaConsumer(ctx context.Context) (Consumer, error) {
	brokers, topic, group, partitions, replicas := kafkaSettings()
	if len(brokers) == 0 {
		return nil, errors.New("не задана переменная KAFKA_BROKERS")
	}
	if err := ensureTopic(ctx, brokers, topic, partitions, replicas); err != nil {
		slog.Warn("не удалось создать топик, полагаюсь на существующий", "err", err)
	}
	// Пауза между коммитами offset-ов. Ноль (по умолчанию) означает
	// синхронный коммит на каждое сообщение: максимальная точность
	// "не потерять" (повторы допустимы), но круговой поход к брокеру на
	// каждое событие. Значение больше нуля включает пакетный коммит.
	commitInterval, err := time.ParseDuration(env("KAFKA_COMMIT_INTERVAL", "0s"))
	if err != nil {
		commitInterval = 0
	}
	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:        brokers,
		Topic:          topic,
		GroupID:        group,
		MinBytes:       1,
		MaxBytes:       10 << 20,
		MaxWait:        500 * time.Millisecond,
		CommitInterval: commitInterval,
		StartOffset:    kafka.FirstOffset,
	})
	return &kafkaConsumer{reader: reader}, nil
}

func (k *kafkaConsumer) Name() string { return "kafka" }

func (k *kafkaConsumer) Consume(ctx context.Context, handle func(context.Context, Event) error) error {
	for {
		msg, err := k.reader.FetchMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			slog.WarnContext(ctx, "ошибка чтения из Kafka", "err", err)
			time.Sleep(time.Second)
			continue
		}
		ev, err := decode(msg.Value)
		if err != nil {
			Consumed.WithLabelValues("kafka", "malformed").Inc()
			return fmt.Errorf("некорректное событие Kafka, раздел %d, смещение %d: %w", msg.Partition, msg.Offset, err)
		}
		if err := handle(ctx, ev); err != nil {
			Consumed.WithLabelValues("kafka", "error").Inc()
			// Следующий commit подтвердил бы и неуспешное сообщение этого раздела.
			// Закрываем reader: супервизор начнет с последнего подтвержденного смещения.
			return fmt.Errorf("не удалось обработать событие Kafka %d: %w", ev.ID, err)
		}
		if err := k.reader.CommitMessages(ctx, msg); err != nil {
			return fmt.Errorf("не удалось подтвердить смещение Kafka: %w", err)
		}
		Consumed.WithLabelValues("kafka", "ok").Inc()
		Lag.WithLabelValues("kafka").Observe(time.Since(ev.CreatedAt).Seconds())
	}
}

func (k *kafkaConsumer) Close() error { return k.reader.Close() }
