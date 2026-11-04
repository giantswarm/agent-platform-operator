// Package kagent lists and deletes an organization's sessions through the
// kagent controller's SessionService, as the operator's ServiceAccount.
//
// The controller authenticates the ServiceAccount's projected token (its
// machine audience) and reviews a listing of every creator's sessions as a
// cluster-wide `list` on sessions.api.kagent.dev, which the agent-platform
// chart grants the operator. The messages are those of kagent's
// proto/kagent/api/v1alpha1/{sessions,common}.proto.
package kagent

import (
	"context"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/giantswarm/agent-platform-operator/internal/grpcwire"
)

const (
	methodListSessions  = "/kagent.api.v1alpha1.SessionService/ListSessions"
	methodDeleteSession = "/kagent.api.v1alpha1.SessionService/DeleteSession"

	// ListSessionsRequest: all_creators = 2, page = 3.
	fieldAllCreators protowire.Number = 2
	fieldPage        protowire.Number = 3
	// PageRequest: limit = 1, page_token = 2.
	fieldLimit     protowire.Number = 1
	fieldPageToken protowire.Number = 2
	// ListSessionsResponse: sessions = 1, page = 2; PageResponse:
	// next_page_token = 1.
	fieldSessions      protowire.Number = 1
	fieldResponsePage  protowire.Number = 2
	fieldNextPageToken protowire.Number = 1
	// Session: id = 1, agent = 15; ResourceReference: namespace = 1.
	fieldSessionID    protowire.Number = 1
	fieldSessionAgent protowire.Number = 15
	fieldNamespace    protowire.Number = 1
	// DeleteSessionRequest: session_id = 1.
	fieldDeleteID protowire.Number = 1

	// pageSize is the controller's cap for ListSessions.
	pageSize = 100
	// maxPages bounds how far a listing is followed, so a misbehaving
	// next_page_token cannot loop forever.
	maxPages = 1000
)

// Sessions is the controller's SessionService.
type Sessions struct{ conn *grpcwire.Conn }

// New calls the SessionService on conn.
func New(conn *grpcwire.Conn) *Sessions { return &Sessions{conn: conn} }

// List returns the IDs of every creator's sessions of the Agents in
// namespace. The service filters by one Agent at most, and the organization's
// Agents are gone by now, so it lists every session and keeps the
// namespace's.
func (s *Sessions) List(ctx context.Context, namespace string) ([]string, error) {
	var ids []string
	token := ""
	for range maxPages {
		page := grpcwire.Message(nil).Uint(fieldLimit, pageSize).String(fieldPageToken, token)
		req := grpcwire.Message(nil).Bool(fieldAllCreators, true).Message(fieldPage, page)
		resp, err := s.conn.Invoke(ctx, methodListSessions, req)
		if err != nil {
			return nil, fmt.Errorf("kagent ListSessions: %w", err)
		}
		fields, err := grpcwire.Fields(resp)
		if err != nil {
			return nil, fmt.Errorf("kagent ListSessions: %w", err)
		}
		for _, raw := range fields[fieldSessions] {
			session, err := grpcwire.Fields(raw)
			if err != nil {
				return nil, fmt.Errorf("kagent ListSessions: %w", err)
			}
			agent, err := grpcwire.Fields(grpcwire.First(session, fieldSessionAgent))
			if err != nil {
				return nil, fmt.Errorf("kagent ListSessions: %w", err)
			}
			if string(grpcwire.First(agent, fieldNamespace)) == namespace {
				ids = append(ids, string(grpcwire.First(session, fieldSessionID)))
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

// Delete deletes one session. The controller ends its runtime
// asynchronously; a session already gone is no error.
func (s *Sessions) Delete(ctx context.Context, id string) error {
	_, err := s.conn.Invoke(ctx, methodDeleteSession, grpcwire.Message(nil).String(fieldDeleteID, id))
	if status.Code(err) == codes.NotFound {
		return nil
	}
	if err != nil {
		return fmt.Errorf("kagent DeleteSession %s: %w", id, err)
	}
	return nil
}
