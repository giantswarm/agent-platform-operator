// Package grpcwiretest serves gRPC methods with encoded messages in process,
// for the tests of the clients built on grpcwire.
package grpcwiretest

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"

	"github.com/giantswarm/agent-platform-operator/internal/grpcwire"
)

// Call is one call the server received.
type Call struct {
	Method        string
	Authorization string
	Request       []byte
}

// Handler answers a call with an encoded response or an error.
type Handler func(Call) ([]byte, error)

// Server serves every method with a Handler and records the calls.
type Server struct {
	mu    sync.Mutex
	calls []Call
}

// Calls are the calls received so far.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

// Start serves handle and returns a Conn to it that sends token as its
// bearer.
func Start(t *testing.T, token string, handle Handler) (*Server, *grpcwire.Conn) {
	t.Helper()
	s := &Server{}
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer(grpc.ForceServerCodecV2(raw{}), grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
		method, _ := grpc.MethodFromServerStream(stream)
		md, _ := metadata.FromIncomingContext(stream.Context())
		var req []byte
		if err := stream.RecvMsg(&req); err != nil {
			return err
		}
		call := Call{Method: method, Request: req}
		if v := md.Get("authorization"); len(v) > 0 {
			call.Authorization = v[0]
		}
		s.mu.Lock()
		s.calls = append(s.calls, call)
		s.mu.Unlock()
		resp, err := handle(call)
		if err != nil {
			return err
		}
		return stream.SendMsg(&resp)
	}))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return s, grpcwire.NewConn(conn, tokenFile)
}

// raw passes encoded messages through.
type raw struct{}

func (raw) Marshal(v any) (mem.BufferSlice, error) {
	b, ok := v.(*[]byte)
	if !ok {
		return nil, fmt.Errorf("cannot marshal %T", v)
	}
	return mem.BufferSlice{mem.SliceBuffer(*b)}, nil
}

func (raw) Unmarshal(data mem.BufferSlice, v any) error {
	b, ok := v.(*[]byte)
	if !ok {
		return fmt.Errorf("cannot unmarshal into %T", v)
	}
	*b = data.Materialize()
	return nil
}

func (raw) Name() string { return "proto" }
