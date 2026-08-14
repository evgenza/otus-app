package audit

import (
	"context"
	"log/slog"
	"os"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"
)

type Entry struct {
	Event string    `bson:"event" json:"event"`
	Text  string    `bson:"text" json:"text"`
	At    time.Time `bson:"at" json:"at"`
}

type Log struct {
	coll *mongo.Collection
}

func New(ctx context.Context) (*Log, error) {
	url := os.Getenv("MONGO_URL")
	if url == "" {
		return nil, nil
	}
	opts := options.Client().
		ApplyURI(url).
		SetWriteConcern(writeconcern.Majority()).
		SetServerSelectionTimeout(5 * time.Second)
	client, err := mongo.Connect(ctx, opts)
	if err != nil {
		return nil, err
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx, nil); err != nil {
		_ = client.Disconnect(context.Background())
		return nil, err
	}
	slog.Info("аудит-лог в MongoDB подключен")
	return &Log{coll: client.Database("otus").Collection("audit")}, nil
}

func (l *Log) Record(ctx context.Context, event, text string) {
	if l == nil {
		return
	}
	insertCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, err := l.coll.InsertOne(insertCtx, Entry{Event: event, Text: text, At: time.Now().UTC()})
	if err != nil {
		slog.WarnContext(ctx, "не удалось записать событие в аудит-лог", "event", event, "err", err)
	}
}

func (l *Log) Last(ctx context.Context, n int64) ([]Entry, error) {
	findCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cursor, err := l.coll.Find(findCtx, bson.D{},
		options.Find().SetSort(bson.D{{Key: "at", Value: -1}}).SetLimit(n))
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0)
	if err := cursor.All(findCtx, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}
