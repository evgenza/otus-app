package broker

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestRabbitHandshakeHonorsCancellation(t *testing.T) {
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
		// Соединение принято, но сервер не отвечает на AMQP handshake.
		<-done
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, _, _, err = openRabbitChannel(ctx, "amqp://otus:otus@"+lis.Addr().String()+"/", "test", true)
	if err == nil {
		t.Fatal("молчащий сервер не должен успешно подключаться")
	}
	if time.Since(started) > time.Second {
		t.Fatal("AMQP handshake проигнорировал таймаут")
	}
}
