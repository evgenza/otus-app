package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/evgenza/otus-app/internal/grpcapi"
	"github.com/evgenza/otus-app/internal/handlers"
	"github.com/evgenza/otus-app/internal/security"
)

func newGRPCClient(addr string) (grpcapi.MessageServiceClient, error) {
	clientTLS, err := security.ClientTLS()
	if err != nil {
		return nil, err
	}
	creds := insecure.NewCredentials()
	if clientTLS != nil {
		creds = credentials.NewTLS(clientTLS.Clone())
	}
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(creds),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
	)
	if err != nil {
		return nil, err
	}
	return grpcapi.NewMessageServiceClient(conn), nil
}

func withAuth(r *http.Request) context.Context {
	ctx := r.Context()
	if token := r.Header.Get("Authorization"); token != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", token)
	}
	return ctx
}

func writeGRPCError(w http.ResponseWriter, err error) {
	st, _ := status.FromError(err)
	httpStatus := http.StatusBadGateway
	switch st.Code() {
	case codes.Unauthenticated:
		httpStatus = http.StatusUnauthorized
	case codes.InvalidArgument:
		httpStatus = http.StatusBadRequest
	case codes.Internal:
		httpStatus = http.StatusInternalServerError
	}
	writeError(w, httpStatus, st.Message())
}

func fromProto(m *grpcapi.Message) handlers.Message {
	return handlers.Message{
		ID:         m.GetId(),
		Text:       m.GetText(),
		Checksum:   m.GetChecksum(),
		ChecksumOK: m.GetChecksumOk(),
		CreatedAt:  m.GetCreatedAt().AsTime(),
	}
}

func (g *gateway) grpcCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "некорректное тело запроса")
		return
	}
	ctx, cancel := context.WithTimeout(withAuth(r), 10*time.Second)
	defer cancel()

	msg, err := g.grpc.CreateMessage(ctx, &grpcapi.CreateMessageRequest{Text: req.Text})
	if err != nil {
		slog.ErrorContext(r.Context(), "gRPC CreateMessage не удался", "err", err)
		writeGRPCError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(fromProto(msg))
}

func (g *gateway) grpcList(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(withAuth(r), 30*time.Second)
	defer cancel()

	stream, err := g.grpc.ListMessages(ctx, &grpcapi.ListMessagesRequest{})
	if err != nil {
		slog.ErrorContext(r.Context(), "gRPC ListMessages не удался", "err", err)
		writeGRPCError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	flusher, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			slog.ErrorContext(r.Context(), "gRPC-стрим оборвался", "err", err)
			return
		}
		_ = enc.Encode(fromProto(msg))
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func (g *gateway) grpcBatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Texts []string `json:"texts"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "некорректное тело запроса")
		return
	}
	if len(req.Texts) == 0 {
		writeError(w, http.StatusBadRequest, "поле texts обязательно")
		return
	}
	ctx, cancel := context.WithTimeout(withAuth(r), 30*time.Second)
	defer cancel()

	stream, err := g.grpc.BatchCreate(ctx)
	if err != nil {
		slog.ErrorContext(r.Context(), "gRPC BatchCreate не удался", "err", err)
		writeGRPCError(w, err)
		return
	}
	for _, text := range req.Texts {
		if err := stream.Send(&grpcapi.CreateMessageRequest{Text: text}); err != nil {
			break
		}
	}
	summary, err := stream.CloseAndRecv()
	if err != nil {
		slog.ErrorContext(r.Context(), "gRPC BatchCreate не удался", "err", err)
		writeGRPCError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"created": summary.GetCreated(),
		"ids":     summary.GetIds(),
	})
}

func (g *gateway) grpcChat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Notes []string `json:"notes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "некорректное тело запроса")
		return
	}
	if len(req.Notes) == 0 {
		writeError(w, http.StatusBadRequest, "поле notes обязательно")
		return
	}
	ctx, cancel := context.WithTimeout(withAuth(r), 30*time.Second)
	defer cancel()

	stream, err := g.grpc.Chat(ctx)
	if err != nil {
		slog.ErrorContext(r.Context(), "gRPC Chat не удался", "err", err)
		writeGRPCError(w, err)
		return
	}

	replies := make([]string, 0, len(req.Notes))
	for _, note := range req.Notes {
		if err := stream.Send(&grpcapi.ChatNote{Text: note}); err != nil {
			writeGRPCError(w, err)
			return
		}
		reply, err := stream.Recv()
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		replies = append(replies, reply.GetText())
	}
	_ = stream.CloseSend()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"replies": replies})
}
