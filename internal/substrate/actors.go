// Package substrate counts an organization's actors through Substrate's
// ate-api Control service, as the operator's ServiceAccount.
//
// An actor is not a Kubernetes object: ate-api keeps it, in the atespace of
// the Agent's namespace, which kagent names its actors' atespace. The messages
// are those of Substrate's pkg/proto/ateapipb/ateapi.proto.
package substrate

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/giantswarm/agent-platform-operator/internal/grpcwire"
)

const (
	methodListActors = "/ateapi.Control/ListActors"

	// ListActorsRequest: atespace = 1, page_size = 2, page_token = 3.
	fieldAtespace  protowire.Number = 1
	fieldPageSize  protowire.Number = 2
	fieldPageToken protowire.Number = 3
	// ListActorsResponse: actors = 1, next_page_token = 2.
	fieldActors        protowire.Number = 1
	fieldNextPageToken protowire.Number = 2

	pageSize = 1000
	maxPages = 100
)

// Actors is ate-api's actor listing.
type Actors struct{ conn *grpcwire.Conn }

// New calls the Control service on conn.
func New(conn *grpcwire.Conn) *Actors { return &Actors{conn: conn} }

// Count is the number of actors in atespace, whatever their state: an actor
// that exists can still upload a snapshot.
func (a *Actors) Count(ctx context.Context, atespace string) (int, error) {
	n := 0
	token := ""
	for range maxPages {
		req := grpcwire.Message(nil).String(fieldAtespace, atespace).Uint(fieldPageSize, pageSize).String(fieldPageToken, token)
		resp, err := a.conn.Invoke(ctx, methodListActors, req)
		if err != nil {
			return 0, fmt.Errorf("substrate ListActors %s: %w", atespace, err)
		}
		fields, err := grpcwire.Fields(resp)
		if err != nil {
			return 0, fmt.Errorf("substrate ListActors %s: %w", atespace, err)
		}
		n += len(fields[fieldActors])
		if token = string(grpcwire.First(fields, fieldNextPageToken)); token == "" {
			return n, nil
		}
	}
	return 0, fmt.Errorf("substrate ListActors %s: %w", atespace, grpcwire.ErrPageLimit)
}
