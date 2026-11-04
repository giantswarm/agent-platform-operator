package substrate_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/giantswarm/agent-platform-operator/internal/grpcwire/grpcwiretest"
	"github.com/giantswarm/agent-platform-operator/internal/substrate"
)

// The messages are encoded with the field numbers of Substrate's
// pkg/proto/ateapipb/ateapi.proto, independently of the client's encoder:
// ListActorsRequest{atespace = 1, page_size = 2, page_token = 3},
// ListActorsResponse{actors = 1, next_page_token = 2}.

func bytesField(b []byte, num protowire.Number, v []byte) []byte {
	b = protowire.AppendTag(b, num, protowire.BytesType)
	return protowire.AppendBytes(b, v)
}

func TestCountFollowsEveryPageOfTheAtespace(t *testing.T) {
	actor := bytesField(nil, 1, bytesField(nil, 2, []byte("actor"))) // Actor{metadata = 1{name = 2}}
	srv, conn := grpcwiretest.Start(t, "operator-token", func(c grpcwiretest.Call) ([]byte, error) {
		if len(c.Request) > 0 && hasField(c.Request, 3) {
			return bytesField(nil, 1, actor), nil
		}
		// A page may be empty even with more to come.
		return bytesField(nil, 2, []byte("page-2")), nil
	})

	n, err := substrate.New(conn).Count(t.Context(), "agents-acme-x7k2q")
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	calls := srv.Calls()
	require.Len(t, calls, 2)
	assert.Equal(t, "/ateapi.Control/ListActors", calls[0].Method)
	assert.Equal(t, "Bearer operator-token", calls[0].Authorization)
	want := bytesField(nil, 1, []byte("agents-acme-x7k2q"))
	want = protowire.AppendVarint(protowire.AppendTag(want, 2, protowire.VarintType), 1000)
	assert.Equal(t, want, calls[0].Request)
	assert.Equal(t, bytesField(want, 3, []byte("page-2")), calls[1].Request)
}

func hasField(b []byte, want protowire.Number) bool {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return false
		}
		if num == want {
			return true
		}
		b = b[n:]
		b = b[protowire.ConsumeFieldValue(num, typ, b):]
	}
	return false
}
