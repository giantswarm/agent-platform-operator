package kagent_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/giantswarm/agent-platform-operator/internal/grpcwire/grpcwiretest"
	"github.com/giantswarm/agent-platform-operator/internal/kagent"
)

// The messages below are encoded with the field numbers of kagent's
// proto/kagent/api/v1alpha1/{sessions,common}.proto, independently of the
// client's encoder.

func bytesField(b []byte, num protowire.Number, v []byte) []byte {
	b = protowire.AppendTag(b, num, protowire.BytesType)
	return protowire.AppendBytes(b, v)
}

// session is a Session{id = 1, agent = 15 ResourceReference{namespace = 1,
// name = 2}}.
func session(id, namespace string) []byte {
	agent := bytesField(bytesField(nil, 1, []byte(namespace)), 2, []byte("triage"))
	return bytesField(bytesField(nil, 1, []byte(id)), 15, agent)
}

// listResponse is a ListSessionsResponse{sessions = 1, page = 2
// PageResponse{next_page_token = 1}}.
func listResponse(next string, sessions ...[]byte) []byte {
	var b []byte
	for _, s := range sessions {
		b = bytesField(b, 1, s)
	}
	if next != "" {
		b = bytesField(b, 2, bytesField(nil, 1, []byte(next)))
	}
	return b
}

// request reads a ListSessionsRequest: all_creators (2), page.limit (3.1),
// page.page_token (3.2).
func request(t *testing.T, b []byte) (allCreators bool, limit uint64, token string) {
	t.Helper()
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		require.GreaterOrEqual(t, n, 0)
		b = b[n:]
		switch {
		case num == 2 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			allCreators, b = v == 1, b[n:]
		case num == 3 && typ == protowire.BytesType:
			page, n := protowire.ConsumeBytes(b)
			b = b[n:]
			for len(page) > 0 {
				pnum, ptyp, pn := protowire.ConsumeTag(page)
				page = page[pn:]
				if pnum == 1 && ptyp == protowire.VarintType {
					v, n := protowire.ConsumeVarint(page)
					limit, page = v, page[n:]
					continue
				}
				v, n := protowire.ConsumeBytes(page)
				token, page = string(v), page[n:]
			}
		default:
			t.Fatalf("ListSessionsRequest carries field %d", num)
		}
	}
	return allCreators, limit, token
}

func TestListReadsEveryCreatorsSessionsOfTheNamespaceOnEveryPage(t *testing.T) {
	srv, conn := grpcwiretest.Start(t, "operator-token", func(c grpcwiretest.Call) ([]byte, error) {
		if _, _, token := request(t, c.Request); token == "" {
			return listResponse("page-2", session("a", "agents-acme-x7k2q"), session("b", "agents-other-abcde")), nil
		}
		return listResponse("", session("c", "agents-acme-x7k2q")), nil
	})

	ids, err := kagent.New(conn).List(t.Context(), "agents-acme-x7k2q")
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "c"}, ids)

	calls := srv.Calls()
	require.Len(t, calls, 2)
	for i, wantToken := range []string{"", "page-2"} {
		assert.Equal(t, "/kagent.api.v1alpha1.SessionService/ListSessions", calls[i].Method)
		assert.Equal(t, "Bearer operator-token", calls[i].Authorization)
		all, limit, token := request(t, calls[i].Request)
		assert.True(t, all, "the listing is not every creator's")
		assert.EqualValues(t, 100, limit)
		assert.Equal(t, wantToken, token)
	}
}

func TestDelete(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		wantErr bool
	}{
		{name: "deletes the session"},
		{name: "a session already gone is no error", err: status.Error(codes.NotFound, "Session not found")},
		{name: "a refusal is an error", err: status.Error(codes.PermissionDenied, "denied"), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, conn := grpcwiretest.Start(t, "operator-token", func(grpcwiretest.Call) ([]byte, error) { return nil, tc.err })
			err := kagent.New(conn).Delete(t.Context(), "11111111-1111-1111-1111-111111111111")
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			calls := srv.Calls()
			require.Len(t, calls, 1)
			assert.Equal(t, "/kagent.api.v1alpha1.SessionService/DeleteSession", calls[0].Method)
			// DeleteSessionRequest{session_id = 1}.
			assert.Equal(t, bytesField(nil, 1, []byte("11111111-1111-1111-1111-111111111111")), calls[0].Request)
		})
	}
}
