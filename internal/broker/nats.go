package broker

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

func natsSettings() (urls []string, stream, subject, durable string, replicas int) {
	urls = envList("NATS_URLS")
	stream = env("NATS_STREAM", "OTUS")
	subject = env("NATS_SUBJECT", "otus.messages")
	durable = env("NATS_DURABLE", "otus-consumer")
	replicas, _ = strconv.Atoi(env("NATS_REPLICAS", "3"))
	return
}

// natsConnect подключается к кластеру и создает поток JetStream: события
// должны переживать перезапуск узла, поэтому хранилище файловое, а поток
// реплицируется на три узла.
func natsConnect(ctx context.Context) (*nats.Conn, jetstream.JetStream, jetstream.Stream, string, error) {
	urls, streamName, subject, _, replicas := natsSettings()
	if len(urls) == 0 {
		return nil, nil, nil, "", nil
	}
	nc, err := nats.Connect(strings.Join(urls, ","),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second),
		nats.Timeout(5*time.Second),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			slog.Warn("соединение с NATS потеряно", "err", err)
		}),
		nats.ReconnectHandler(func(c *nats.Conn) {
			slog.Info("переподключился к NATS", "url", c.ConnectedUrl())
		}),
	)
	if err != nil {
		return nil, nil, nil, "", err
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, nil, nil, "", err
	}
	createCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	stream, err := js.CreateOrUpdateStream(createCtx, jetstream.StreamConfig{
		Name:      streamName,
		Subjects:  []string{subject},
		Storage:   jetstream.FileStorage,
		Retention: jetstream.LimitsPolicy,
		Replicas:  replicas,
	})
	if err != nil {
		nc.Close()
		return nil, nil, nil, "", err
	}
	return nc, js, stream, subject, nil
}

type natsPublisher struct {
	nc      *nats.Conn
	js      jetstream.JetStream
	subject string
}

func newNATSPublisher(ctx context.Context) (Publisher, error) {
	nc, js, _, subject, err := natsConnect(ctx)
	if err != nil || nc == nil {
		return nil, err
	}
	return &natsPublisher{nc: nc, js: js, subject: subject}, nil
}

func (n *natsPublisher) Name() string { return "nats" }

func (n *natsPublisher) Publish(ctx context.Context, ev Event) error {
	payload, err := encode(ev)
	if err != nil {
		return err
	}
	// Publish ждет подтверждения от лидера потока (аналог acks=all у Kafka).
	_, err = n.js.Publish(ctx, n.subject, payload,
		jetstream.WithMsgID(strconv.FormatInt(ev.ID, 10)))
	return err
}

func (n *natsPublisher) Close() error {
	n.nc.Close()
	return nil
}

type natsConsumer struct {
	nc       *nats.Conn
	consumer jetstream.Consumer
}

func newNATSConsumer(ctx context.Context) (Consumer, error) {
	nc, _, stream, _, err := natsConnect(ctx)
	if err != nil {
		return nil, err
	}
	if nc == nil {
		return nil, errors.New("не задана переменная NATS_URLS")
	}
	_, _, _, durable, _ := natsSettings()
	createCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	// Durable pull-консьюмер: все реплики воркера читают одну подписку и
	// делят сообщения между собой, как consumer group в Kafka.
	consumer, err := stream.CreateOrUpdateConsumer(createCtx, jetstream.ConsumerConfig{
		Durable:       durable,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       30 * time.Second,
		MaxAckPending: 512,
	})
	if err != nil {
		nc.Close()
		return nil, err
	}
	return &natsConsumer{nc: nc, consumer: consumer}, nil
}

func (n *natsConsumer) Name() string { return "nats" }

func (n *natsConsumer) Consume(ctx context.Context, handle func(context.Context, Event) error) error {
	iter, err := n.consumer.Messages(jetstream.PullMaxMessages(64))
	if err != nil {
		return err
	}
	defer iter.Stop()

	go func() {
		<-ctx.Done()
		iter.Stop()
	}()

	for ctx.Err() == nil {
		msg, err := iter.Next()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, jetstream.ErrMsgIteratorClosed) {
				return nil
			}
			slog.WarnContext(ctx, "ошибка чтения из NATS", "err", err)
			time.Sleep(time.Second)
			continue
		}
		ev, decodeErr := decode(msg.Data())
		if decodeErr != nil {
			Consumed.WithLabelValues("nats", "malformed").Inc()
			_ = msg.Term()
			continue
		}
		if err := handle(ctx, ev); err != nil {
			Consumed.WithLabelValues("nats", "error").Inc()
			slog.WarnContext(ctx, "не удалось обработать событие", "broker", "nats", "id", ev.ID, "err", err)
			_ = msg.Nak() // вернуть в поток и получить снова
			continue
		}
		Consumed.WithLabelValues("nats", "ok").Inc()
		Lag.WithLabelValues("nats").Observe(time.Since(ev.CreatedAt).Seconds())
		_ = msg.Ack()
	}
	return nil
}

func (n *natsConsumer) Close() error {
	n.nc.Close()
	return nil
}
