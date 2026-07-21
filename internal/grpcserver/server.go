package grpcserver

import (
	"context"
	"errors"
	"io"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/evgenza/otus-app/internal/grpcapi"
	"github.com/evgenza/otus-app/internal/handlers"
	"github.com/evgenza/otus-app/internal/observability"
	"github.com/evgenza/otus-app/internal/security"
)

func New(store handlers.MessageStore, auth *security.Auth, creds credentials.TransportCredentials) *grpc.Server {
	opts := []grpc.ServerOption{
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		grpc.UnaryInterceptor(authUnary(auth)),
		grpc.StreamInterceptor(authStream(auth)),
	}
	if creds != nil {
		opts = append(opts, grpc.Creds(creds))
	}
	srv := grpc.NewServer(opts...)
	grpcapi.RegisterMessageServiceServer(srv, &service{store: store})
	return srv
}

var protectedMethods = map[string]bool{
	grpcapi.MessageService_CreateMessage_FullMethodName: true,
	grpcapi.MessageService_BatchCreate_FullMethodName:   true,
}

func authUnary(auth *security.Auth) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := checkAuth(ctx, auth, info.FullMethod); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

func authStream(auth *security.Auth) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := checkAuth(ss.Context(), auth, info.FullMethod); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}

func checkAuth(ctx context.Context, auth *security.Auth, method string) error {
	if auth == nil || !protectedMethods[method] {
		return nil
	}
	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get("authorization")
	if len(values) == 0 {
		return status.Error(codes.Unauthenticated, "требуется токен авторизации")
	}
	raw, found := strings.CutPrefix(values[0], "Bearer ")
	if !found || raw == "" {
		return status.Error(codes.Unauthenticated, "требуется токен авторизации")
	}
	if err := auth.Validate(raw); err != nil {
		return status.Error(codes.Unauthenticated, "недействительный токен")
	}
	return nil
}

type service struct {
	grpcapi.UnimplementedMessageServiceServer
	store handlers.MessageStore
}

func (s *service) CreateMessage(ctx context.Context, req *grpcapi.CreateMessageRequest) (*grpcapi.Message, error) {
	if strings.TrimSpace(req.GetText()) == "" {
		return nil, status.Error(codes.InvalidArgument, "поле text обязательно")
	}
	msg, err := s.store.Create(ctx, req.GetText())
	if err != nil {
		return nil, status.Error(codes.Internal, "не удалось сохранить сообщение")
	}
	observability.MessagesCreated.Inc()
	return toProto(msg), nil
}

func (s *service) ListMessages(req *grpcapi.ListMessagesRequest, stream grpc.ServerStreamingServer[grpcapi.Message]) error {
	msgs, err := s.store.List(stream.Context())
	if err != nil {
		return status.Error(codes.Internal, "не удалось получить сообщения")
	}
	if limit := int(req.GetLimit()); limit > 0 && limit < len(msgs) {
		msgs = msgs[:limit]
	}
	for _, m := range msgs {
		if err := stream.Send(toProto(m)); err != nil {
			return err
		}
	}
	return nil
}

func (s *service) BatchCreate(stream grpc.ClientStreamingServer[grpcapi.CreateMessageRequest, grpcapi.BatchCreateSummary]) error {
	var ids []int64
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return stream.SendAndClose(&grpcapi.BatchCreateSummary{
				Created: int32(len(ids)),
				Ids:     ids,
			})
		}
		if err != nil {
			return err
		}
		if strings.TrimSpace(req.GetText()) == "" {
			return status.Error(codes.InvalidArgument, "поле text обязательно")
		}
		msg, err := s.store.Create(stream.Context(), req.GetText())
		if err != nil {
			return status.Error(codes.Internal, "не удалось сохранить сообщение")
		}
		observability.MessagesCreated.Inc()
		ids = append(ids, msg.ID)
	}
}

func (s *service) Chat(stream grpc.BidiStreamingServer[grpcapi.ChatNote, grpcapi.ChatNote]) error {
	for {
		note, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		reply := &grpcapi.ChatNote{Text: "эхо: " + note.GetText()}
		if err := stream.Send(reply); err != nil {
			return err
		}
	}
}

func toProto(m handlers.Message) *grpcapi.Message {
	return &grpcapi.Message{
		Id:         m.ID,
		Text:       m.Text,
		Checksum:   m.Checksum,
		ChecksumOk: m.ChecksumOK,
		CreatedAt:  timestamppb.New(m.CreatedAt),
	}
}
