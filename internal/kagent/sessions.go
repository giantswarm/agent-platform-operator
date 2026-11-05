// Package kagent lists and deletes an organization's sessions through the
// kagent controller's SessionService, as the operator's ServiceAccount.
//
// The controller authenticates the ServiceAccount's projected token (its
// machine audience) and reviews a listing of every creator's sessions as a
// cluster-wide `list` on sessions.api.kagent.dev; the agent-platform chart
// grants the operator `list` and `delete` there. The messages are those of
// kagent's proto/kagent/api/{v1,v1alpha1}/{sessions,common}.proto: the
// SessionService of either package, whichever the controller serves.
package kagent

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/giantswarm/agent-platform-operator/internal/grpcwire"
)

const (
	// ListSessionsRequest: all_creators = 2, page = 3, namespace = 7.
	fieldAllCreators   protowire.Number = 2
	fieldPage          protowire.Number = 3
	fieldListNamespace protowire.Number = 7
	// PageRequest: limit = 1, page_token = 2.
	fieldLimit     protowire.Number = 1
	fieldPageToken protowire.Number = 2
	// ListSessionsResponse: sessions = 1, page = 2; PageResponse:
	// next_page_token = 1.
	fieldSessions      protowire.Number = 1
	fieldResponsePage  protowire.Number = 2
	fieldNextPageToken protowire.Number = 1
	// Session: id = 1, agent = 15; AgentProvenance: ref = 1;
	// ResourceReference: namespace = 1.
	fieldSessionID    protowire.Number = 1
	fieldSessionAgent protowire.Number = 15
	fieldAgentRef     protowire.Number = 1
	fieldNamespace    protowire.Number = 1
	// DeleteSessionRequest: session_id = 1.
	fieldDeleteID protowire.Number = 1

	// pageSize is the controller's cap for ListSessions.
	pageSize = 100
	// maxPages bounds how far a listing is followed, so a misbehaving
	// next_page_token cannot loop forever.
	maxPages = 1000
)

// api is one package of kagent's SessionService. The two packages share every
// field number the erasure uses but two: Session.agent (15), and the
// namespace filter (ListSessionsRequest.namespace = 7), which only the
// v1alpha1 kagent line of giantswarm/kagent-upstream carries.
type api struct {
	pkg string
	// provenance: Session.agent is an AgentProvenance whose ref (1) is the
	// ResourceReference; otherwise it is the ResourceReference.
	provenance bool
	// namespaceFilter: the package's ListSessionsRequest has namespace = 7.
	namespaceFilter bool
}

func (a api) method(name string) string { return "/" + a.pkg + ".SessionService/" + name }

// apis are the packages tried, in order, while the controller answers
// Unimplemented.
var apis = [...]api{
	{pkg: "kagent.api.v1", provenance: true},
	{pkg: "kagent.api.v1alpha1", namespaceFilter: true},
}

// Sessions is the controller's SessionService.
type Sessions struct {
	conn *grpcwire.Conn
	// current indexes the api the controller last answered, tried first.
	current atomic.Int32
}

// New calls the SessionService on conn.
func New(conn *grpcwire.Conn) *Sessions { return &Sessions{conn: conn} }

// invoke calls method on the api the controller last answered, and on the
// others while it answers Unimplemented, so a kagent upgraded or rolled back
// across the two packages is followed without a restart.
func (s *Sessions) invoke(ctx context.Context, method string, req func(api) []byte) ([]byte, api, error) {
	first := int(s.current.Load())
	var unimplemented []error
	for i := range len(apis) {
		n := (first + i) % len(apis)
		resp, err := s.conn.Invoke(ctx, apis[n].method(method), req(apis[n]))
		if status.Code(err) == codes.Unimplemented {
			unimplemented = append(unimplemented, err)
			continue
		}
		s.current.Store(int32(n))
		return resp, apis[n], err
	}
	return nil, api{}, fmt.Errorf("the controller serves no SessionService of %s or %s: %w", apis[0].pkg, apis[1].pkg, errors.Join(unimplemented...))
}

// List returns the IDs of every creator's sessions of the Agents in
// namespace, Agents that are gone included. On kagent.api.v1alpha1 the
// listing asks for the namespace, which kagent matches against each
// session's pinned revision; a kagent without the filter ignores it and lists
// every namespace's sessions, as kagent.api.v1 does, so the namespace's are
// kept here too.
func (s *Sessions) List(ctx context.Context, namespace string) ([]string, error) {
	var ids []string
	token := ""
	for range maxPages {
		request := func(a api) []byte {
			page := grpcwire.Message(nil).Uint(fieldLimit, pageSize).String(fieldPageToken, token)
			msg := grpcwire.Message(nil).Bool(fieldAllCreators, true).Message(fieldPage, page)
			if a.namespaceFilter {
				msg = msg.String(fieldListNamespace, namespace)
			}
			return msg
		}
		resp, served, err := s.invoke(ctx, "ListSessions", request)
		if err != nil {
			return nil, fmt.Errorf("kagent ListSessions: %w", err)
		}
		fields, err := grpcwire.Fields(resp)
		if err != nil {
			return nil, fmt.Errorf("kagent ListSessions: %w", err)
		}
		for _, raw := range fields[fieldSessions] {
			id, of, err := sessionOf(raw, served)
			if err != nil {
				return nil, fmt.Errorf("kagent ListSessions: %w", err)
			}
			if of == namespace {
				ids = append(ids, id)
			}
		}
		next, err := grpcwire.Fields(grpcwire.First(fields, fieldResponsePage))
		if err != nil {
			return nil, fmt.Errorf("kagent ListSessions: %w", err)
		}
		if token = string(grpcwire.First(next, fieldNextPageToken)); token == "" {
			return ids, nil
		}
	}
	return nil, fmt.Errorf("kagent ListSessions: %w", grpcwire.ErrPageLimit)
}

// sessionOf reads a Session's ID and its Agent's namespace, with Session.agent as a encodes it.
func sessionOf(raw []byte, a api) (id, namespace string, err error) {
	session, err := grpcwire.Fields(raw)
	if err != nil {
		return "", "", err
	}
	ref := grpcwire.First(session, fieldSessionAgent)
	if a.provenance {
		provenance, err := grpcwire.Fields(ref)
		if err != nil {
			return "", "", err
		}
		ref = grpcwire.First(provenance, fieldAgentRef)
	}
	fields, err := grpcwire.Fields(ref)
	if err != nil {
		return "", "", err
	}
	return string(grpcwire.First(session, fieldSessionID)), string(grpcwire.First(fields, fieldNamespace)), nil
}

// Delete deletes one session; the controller ends its runtime
// asynchronously.
//
// NotFound is an error, not "already gone": the session was just listed, and
// kagent's session service reads a session to delete as the caller's own
// (WHERE user_id = caller) unless the caller is its control plane, so it
// answers NotFound for another creator's session. Until kagent authorizes a
// delete of any creator's session for a machine caller with cluster-wide
// `delete` on sessions (giantswarm/kagent-extensions#27, in the kagent line),
// the erasure stops at its Sessions step and says so.
func (s *Sessions) Delete(ctx context.Context, id string) error {
	_, _, err := s.invoke(ctx, "DeleteSession", func(api) []byte { return grpcwire.Message(nil).String(fieldDeleteID, id) })
	if status.Code(err) == codes.NotFound {
		return fmt.Errorf("kagent DeleteSession %s: listed but not found: deleted meanwhile, or kagent refuses another creator's session to this caller: %w", id, err)
	}
	if err != nil {
		return fmt.Errorf("kagent DeleteSession %s: %w", id, err)
	}
	return nil
}
