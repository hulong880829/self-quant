package gateway

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/timestamppb"
	accountv1 "selfquant/backend/gen/account/v1"
	aiv1 "selfquant/backend/gen/ai/v1"
	fundingv1 "selfquant/backend/gen/funding/v1"
)

type testAIAccountServer struct {
	accountv1.UnimplementedAccountServiceServer
	credential *accountv1.AICredential
}

func (s *testAIAccountServer) UpsertAICredential(
	_ context.Context,
	request *accountv1.UpsertAICredentialRequest,
) (*accountv1.AICredentialResponse, error) {
	s.credential = &accountv1.AICredential{
		Provider: request.GetProvider(), ApiKeyMasked: "sk-o****test",
		Status: "unknown", UpdatedAt: timestamppb.Now(),
	}
	return &accountv1.AICredentialResponse{Credential: s.credential}, nil
}

func (s *testAIAccountServer) DeleteAICredential(
	context.Context,
	*accountv1.DeleteAICredentialRequest,
) (*accountv1.DeleteAICredentialResponse, error) {
	s.credential = nil
	return &accountv1.DeleteAICredentialResponse{}, nil
}

type testAIServer struct {
	aiv1.UnimplementedAIServiceServer
}

func (s *testAIServer) GetCredentialStatus(
	context.Context,
	*aiv1.GetCredentialStatusRequest,
) (*aiv1.CredentialStatusResponse, error) {
	return &aiv1.CredentialStatusResponse{Credential: &aiv1.CredentialStatus{
		Provider: "openrouter", ApiKeyMasked: "sk-o****test", Status: "valid",
	}}, nil
}

func (s *testAIServer) TestCredential(
	context.Context,
	*aiv1.TestCredentialRequest,
) (*aiv1.CredentialStatusResponse, error) {
	return &aiv1.CredentialStatusResponse{Credential: &aiv1.CredentialStatus{
		Provider: "openrouter", ApiKeyMasked: "sk-o****test", Status: "valid",
	}}, nil
}

func (s *testAIServer) ListConversations(
	context.Context,
	*aiv1.ListConversationsRequest,
) (*aiv1.ListConversationsResponse, error) {
	return &aiv1.ListConversationsResponse{
		Items: []*aiv1.Conversation{{
			Id:    "11111111-1111-1111-1111-111111111111",
			Title: "BTC 分析", ModelAlias: "free-general",
			LastMessageAt: timestamppb.Now(), ExpiresAt: timestamppb.Now(),
			CreatedAt: timestamppb.Now(), UpdatedAt: timestamppb.Now(),
		}},
	}, nil
}

func (s *testAIServer) CreateConversation(
	context.Context,
	*aiv1.CreateConversationRequest,
) (*aiv1.ConversationResponse, error) {
	return &aiv1.ConversationResponse{Conversation: &aiv1.Conversation{
		Id:         "11111111-1111-1111-1111-111111111111",
		ModelAlias: "free-general", LastMessageAt: timestamppb.Now(),
		ExpiresAt: timestamppb.Now(), CreatedAt: timestamppb.Now(),
		UpdatedAt: timestamppb.Now(),
	}}, nil
}

func (s *testAIServer) ListMessages(
	context.Context,
	*aiv1.ListMessagesRequest,
) (*aiv1.ListMessagesResponse, error) {
	return &aiv1.ListMessagesResponse{Items: []*aiv1.ConversationMessage{{
		Id:             "22222222-2222-2222-2222-222222222222",
		ConversationId: "11111111-1111-1111-1111-111111111111",
		Role:           "user", Status: "completed", Content: "question",
		CreatedAt: timestamppb.Now(), UpdatedAt: timestamppb.Now(),
	}}}, nil
}

func (s *testAIServer) StreamChat(
	_ *aiv1.StreamChatRequest,
	stream aiv1.AIService_StreamChatServer,
) error {
	for _, event := range []*aiv1.StreamChatEvent{
		{Type: "turn", ConversationId: "11111111-1111-1111-1111-111111111111"},
		{Type: "tool", Tool: "funding"},
		{Type: "delta", Delta: "hello"},
		{Type: "done"},
	} {
		if err := stream.Send(event); err != nil {
			return err
		}
	}
	return nil
}

func testRouterWithAI(t *testing.T) http.Handler {
	t.Helper()
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	fundingv1.RegisterFundingServiceServer(server, &testFundingServer{})
	accountv1.RegisterAccountServiceServer(server, &testAIAccountServer{})
	aiv1.RegisterAIServiceServer(server, &testAIServer{})
	healthServer := health.NewServer()
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	grpc_health_v1.RegisterHealthServer(server, healthServer)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	connection, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return listener.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return NewRouter(
		fundingv1.NewFundingServiceClient(connection),
		accountv1.NewAccountServiceClient(connection),
		grpc_health_v1.NewHealthClient(connection),
		Options{
			SessionCookieName: "sq_session", SessionCookieMaxAge: time.Hour,
			AI: aiv1.NewAIServiceClient(connection),
		},
	)
}

func TestAIRoutesRequireSession(t *testing.T) {
	router := testRouterWithAI(t)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/ai/credentials/openrouter", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestAICredentialBindAndTest(t *testing.T) {
	router := testRouterWithAI(t)
	for _, test := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/api/v1/ai/credentials/openrouter", `{"apiKey":"secret"}`},
		{http.MethodPost, "/api/v1/ai/credentials/openrouter/test", ""},
	} {
		request := httptest.NewRequest(test.method, test.path, bytes.NewBufferString(test.body))
		request.AddCookie(&http.Cookie{Name: "sq_session", Value: "token"})
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", test.path, recorder.Code, recorder.Body.String())
		}
	}
}

func TestAIChatStreamsNamedSSEEvents(t *testing.T) {
	router := testRouterWithAI(t)
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/ai/chat",
		strings.NewReader(`{"conversationId":"11111111-1111-1111-1111-111111111111","prompt":"question","clientMessageId":"22222222-2222-2222-2222-222222222222"}`),
	)
	request.AddCookie(&http.Cookie{Name: "sq_session", Value: "token"})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	body := recorder.Body.String()
	if recorder.Header().Get("Content-Type") != "text/event-stream" ||
		!strings.Contains(body, "event: tool") ||
		!strings.Contains(body, `"delta":"hello"`) ||
		!strings.Contains(body, "event: done") {
		t.Fatalf("headers=%v body=%s", recorder.Header(), body)
	}
}

func TestAIConversationRoutes(t *testing.T) {
	router := testRouterWithAI(t)
	for _, test := range []struct {
		method string
		path   string
		body   string
		status int
		match  string
	}{
		{http.MethodGet, "/api/v1/ai/conversations", "", http.StatusOK, "BTC 分析"},
		{http.MethodPost, "/api/v1/ai/conversations", `{}`, http.StatusCreated, "free-general"},
		{
			http.MethodGet,
			"/api/v1/ai/conversations/11111111-1111-1111-1111-111111111111/messages",
			"", http.StatusOK, "question",
		},
	} {
		request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
		request.AddCookie(&http.Cookie{Name: "sq_session", Value: "token"})
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Code != test.status || !strings.Contains(recorder.Body.String(), test.match) {
			t.Fatalf("%s status=%d body=%s", test.path, recorder.Code, recorder.Body.String())
		}
	}
}

func TestAIConversationLimitErrorsAreActionableConflicts(t *testing.T) {
	handler := &Handler{}
	for _, test := range []struct {
		name    string
		message string
		want    string
	}{
		{
			name: "conversation limit", message: "ai conversation limit reached",
			want: "最多保留 5 个会话，请先删除一个",
		},
		{
			name: "raw message limit", message: "ai conversation raw message limit reached",
			want: "当前会话已达到 1000 条原文上限，请新建会话",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.writeAIError(
				recorder, status.Error(codes.ResourceExhausted, test.message),
			)
			if recorder.Code != http.StatusConflict ||
				!strings.Contains(recorder.Body.String(), test.want) {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if public := aiPublicError(
				status.Error(codes.ResourceExhausted, test.message),
			); public != test.want {
				t.Fatalf("public error=%q", public)
			}
		})
	}
}
