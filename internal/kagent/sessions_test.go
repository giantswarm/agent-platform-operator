package kagent_test

import (
	"strings"
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
// proto/kagent/api/{v1,v1alpha1}/{sessions,common}.proto, independently of
// the client's encoder.

const (
	v1       = "/kagent.api.v1.SessionService/"
	v1alpha1 = "/kagent.api.v1alpha1.SessionService/"
)

func bytesField(b []byte, num protowire.Number, v []byte) []byte {
	b = protowire.AppendTag(b, num, protowire.BytesType)
	return protowire.AppendBytes(b, v)
}

// session is a kagent.api.v1 Session{id = 1, agent = 15
// AgentProvenance{ref = 1 ResourceReference{namespace = 1, name = 2},
// uid = 2}}.
func session(id, namespace string) []byte {
	agent := bytesField(bytesField(nil, 1, reference(namespace)), 2, []byte("8b0c2f4e-uid"))
	return bytesField(bytesField(nil, 1, []byte(id)), 15, agent)
}

// sessionV1alpha1 is a kagent.api.v1alpha1 Session{id = 1, agent = 15
// ResourceReference{namespace = 1, name = 2}}.
func sessionV1alpha1(id, namespace string) []byte {
	return bytesField(bytesField(nil, 1, []byte(id)), 15, reference(namespace))
}

func reference(namespace string) []byte {
	return bytesField(bytesField(nil, 1, []byte(namespace)), 2, []byte("triage"))
}

// serving answers the methods of the packages in served, from respond, and
// Unimplemented to the others.
func serving(served *[]string, respond grpcwiretest.Handler) grpcwiretest.Handler {
	return func(c grpcwiretest.Call) ([]byte, error) {
		for _, prefix := range *served {
			if strings.HasPrefix(c.Method, prefix) {
				return respond(c)
			}
		}
		return nil, status.Errorf(codes.Unimplemented, "unknown service %s", c.Method)
	}
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
// page.page_token (3.2), namespace (7).
func request(t *testing.T, b []byte) (allCreators bool, limit uint64, token, namespace string) {
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
		case num == 7 && typ == protowire.BytesType:
			v, n := protowire.ConsumeBytes(b)
			namespace, b = string(v), b[n:]
		default:
			t.Fatalf("ListSessionsRequest carries field %d", num)
		}
	}
	return allCreators, limit, token, namespace
}

func TestListReadsEveryCreatorsSessionsOfTheNamespaceOnEveryPage(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prefix  string
		session func(id, namespace string) []byte
		// filter: the listing asks for the namespace.
		filter bool
	}{
		{name: "kagent.api.v1", prefix: v1, session: session},
		{name: "kagent.api.v1alpha1", prefix: v1alpha1, session: sessionV1alpha1, filter: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			served := []string{tc.prefix}
			srv, conn := grpcwiretest.Start(t, "operator-token", serving(&served, func(c grpcwiretest.Call) ([]byte, error) {
				if _, _, token, _ := request(t, c.Request); token == "" {
					return listResponse("page-2", tc.session("a", "agents-acme-x7k2q"), tc.session("b", "agents-other-abcde")), nil
				}
				return listResponse("", tc.session("c", "agents-acme-x7k2q")), nil
			}))

			ids, err := kagent.New(conn).List(t.Context(), "agents-acme-x7k2q")
			require.NoError(t, err)
			assert.Equal(t, []string{"a", "c"}, ids)

			var calls []grpcwiretest.Call
			for _, c := range srv.Calls() {
				if strings.HasPrefix(c.Method, tc.prefix) {
					calls = append(calls, c)
				}
			}
			require.Len(t, calls, 2)
			for i, wantToken := range []string{"", "page-2"} {
				assert.Equal(t, tc.prefix+"ListSessions", calls[i].Method)
				assert.Equal(t, "Bearer operator-token", calls[i].Authorization)
				all, limit, token, namespace := request(t, calls[i].Request)
				assert.True(t, all, "the listing is not every creator's")
				if tc.filter {
					assert.Equal(t, "agents-acme-x7k2q", namespace, "the listing is not the namespace's")
				} else {
					assert.Empty(t, namespace, "the listing sends a namespace filter the package does not define")
				}
				assert.EqualValues(t, 100, limit)
				assert.Equal(t, wantToken, token)
			}
		})
	}
}

func TestListFollowsTheSessionServicePackageTheControllerServes(t *testing.T) {
	served := []string{v1alpha1}
	srv, conn := grpcwiretest.Start(t, "operator-token", serving(&served, func(c grpcwiretest.Call) ([]byte, error) {
		if strings.HasPrefix(c.Method, v1) {
			return listResponse("", session("on-v1", "agents-acme-x7k2q")), nil
		}
		return listResponse("", sessionV1alpha1("on-v1alpha1", "agents-acme-x7k2q")), nil
	}))
	sessions := kagent.New(conn)
	methods := func() []string {
		var out []string
		for _, c := range srv.Calls() {
			out = append(out, c.Method)
		}
		return out
	}

	ids, err := sessions.List(t.Context(), "agents-acme-x7k2q")
	require.NoError(t, err)
	assert.Equal(t, []string{"on-v1alpha1"}, ids)
	assert.Equal(t, []string{v1 + "ListSessions", v1alpha1 + "ListSessions"}, methods(), "a controller without kagent.api.v1 is not asked on kagent.api.v1alpha1")

	_, err = sessions.List(t.Context(), "agents-acme-x7k2q")
	require.NoError(t, err)
	require.NoError(t, sessions.Delete(t.Context(), "11111111-1111-1111-1111-111111111111"))
	assert.Equal(t, []string{v1alpha1 + "ListSessions", v1alpha1 + "DeleteSession"}, methods()[2:], "the package the controller answered is not tried first")

	// kagent upgraded to kagent.api.v1.
	served = []string{v1}
	ids, err = sessions.List(t.Context(), "agents-acme-x7k2q")
	require.NoError(t, err)
	assert.Equal(t, []string{"on-v1"}, ids)
	assert.Equal(t, []string{v1alpha1 + "ListSessions", v1 + "ListSessions"}, methods()[4:])

	// Rolled back to kagent.api.v1alpha1.
	served = []string{v1alpha1}
	ids, err = sessions.List(t.Context(), "agents-acme-x7k2q")
	require.NoError(t, err)
	assert.Equal(t, []string{"on-v1alpha1"}, ids)

	// Neither.
	served = nil
	_, err = sessions.List(t.Context(), "agents-acme-x7k2q")
	require.Error(t, err)
	assert.Equal(t, codes.Unimplemented, status.Code(err))
}

func TestDelete(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		wantErr bool
	}{
		{name: "deletes the session"},
		{name: "a listed session kagent does not find is an error", err: status.Error(codes.NotFound, "Session not found"), wantErr: true},
		{name: "a refusal is an error", err: status.Error(codes.PermissionDenied, "denied"), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			served := []string{v1}
			srv, conn := grpcwiretest.Start(t, "operator-token", serving(&served, func(grpcwiretest.Call) ([]byte, error) { return nil, tc.err }))
			err := kagent.New(conn).Delete(t.Context(), "11111111-1111-1111-1111-111111111111")
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			calls := srv.Calls()
			require.Len(t, calls, 1)
			assert.Equal(t, v1+"DeleteSession", calls[0].Method)
			// DeleteSessionRequest{session_id = 1}.
			assert.Equal(t, bytesField(nil, 1, []byte("11111111-1111-1111-1111-111111111111")), calls[0].Request)
		})
	}
}
