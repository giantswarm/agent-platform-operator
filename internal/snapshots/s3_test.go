package snapshots_test

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/agent-platform-operator/internal/snapshots"
)

// fakeS3 serves ListObjectsV2 from keys, two to a page, and DeleteObjects,
// refusing the keys in refuse.
type fakeS3 struct {
	mu      sync.Mutex
	keys    []string
	refuse  map[string]bool
	deleted []string
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path != "/agent-snapshots" || !strings.Contains(r.Header.Get("Authorization"), "Credential=id/") {
		http.Error(w, "unexpected bucket path or credentials", http.StatusBadRequest)
		return
	}
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodGet && q.Get("list-type") == "2":
		// S3 lists in key order.
		keys := slices.Sorted(slices.Values(f.keys))
		var page []string
		for _, k := range keys {
			if strings.HasPrefix(k, q.Get("prefix")) && k > q.Get("continuation-token") {
				page = append(page, k)
			}
		}
		truncated := len(page) > 2
		if truncated {
			page = page[:2]
		}
		_, _ = fmt.Fprintf(w, `<ListBucketResult><IsTruncated>%t</IsTruncated>`, truncated)
		for _, k := range page {
			_, _ = fmt.Fprintf(w, `<Contents><Key>%s</Key></Contents>`, k)
		}
		if truncated {
			_, _ = fmt.Fprintf(w, `<NextContinuationToken>%s</NextContinuationToken>`, page[1])
		}
		_, _ = fmt.Fprint(w, `</ListBucketResult>`)
	case r.Method == http.MethodPost && q.Has("delete"):
		body, _ := io.ReadAll(r.Body)
		var request struct {
			Objects []struct{ Key string } `xml:"Object"`
		}
		if err := xml.Unmarshal(body, &request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_, _ = fmt.Fprint(w, `<DeleteResult>`)
		for _, o := range request.Objects {
			if f.refuse[o.Key] {
				_, _ = fmt.Fprintf(w, `<Error><Key>%s</Key><Code>AccessDenied</Code><Message>Access Denied</Message></Error>`, o.Key)
				continue
			}
			f.deleted = append(f.deleted, o.Key)
		}
		_, _ = fmt.Fprint(w, `</DeleteResult>`)
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.String(), http.StatusBadRequest)
	}
}

func client(t *testing.T, f *fakeS3) *snapshots.S3 {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	t.Setenv("AWS_REGION", "eu-west-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "id")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("AWS_SESSION_TOKEN", "")
	client, err := snapshots.NewS3(t.Context(), srv.URL, true)
	require.NoError(t, err)
	return client
}

func TestDeletePrefixDeletesEveryObjectUnderThePrefixOnly(t *testing.T) {
	f := &fakeS3{keys: []string{
		"0b3c/kagent/atespaces/agents-acme/a/1", "0b3c/kagent/atespaces/agents-acme/a/2",
		"0b3c/kagent/atespaces/agents-acme/b/1", "0b3c/agents/triage/1",
		"0b3c-sibling/kagent/1", "0b3d/kagent/1",
	}}
	require.NoError(t, client(t, f).DeletePrefix(t.Context(), "s3://agent-snapshots/0b3c"))
	assert.ElementsMatch(t, f.keys[:4], f.deleted)
}

func TestDeletePrefixReportsAKeyTheBucketRefused(t *testing.T) {
	f := &fakeS3{keys: []string{"0b3c/kagent/1", "0b3c/kagent/2"}, refuse: map[string]bool{"0b3c/kagent/2": true}}
	err := client(t, f).DeletePrefix(t.Context(), "s3://agent-snapshots/0b3c")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "s3://agent-snapshots/0b3c/kagent/2")
}

func TestDeletePrefixRefusesAPrefixWithoutAPath(t *testing.T) {
	for _, prefix := range []string{"s3://agent-snapshots", "s3://agent-snapshots/", "agent-snapshots/0b3c"} {
		require.Error(t, client(t, &fakeS3{}).DeletePrefix(t.Context(), prefix), prefix)
	}
}

func TestDefaultEndpointUsesSDKEnvironment(t *testing.T) {
	store := &fakeS3{keys: []string{"organization/snapshot"}}
	server := httptest.NewServer(store)
	t.Cleanup(server.Close)
	t.Setenv("AWS_ENDPOINT_URL_S3", server.URL)
	t.Setenv("AWS_REGION", "eu-west-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "id")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("AWS_SESSION_TOKEN", "")
	client, err := snapshots.NewS3(t.Context(), "", false)
	require.NoError(t, err)
	require.NoError(t, client.DeletePrefix(t.Context(), "s3://agent-snapshots/organization"))
	require.Equal(t, store.keys, store.deleted)
}

func TestDeletePrefixReportsProtocolFailures(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			store := &fakeS3{keys: []string{"organization/snapshot"}}
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method == method {
					writer.WriteHeader(http.StatusForbidden)
					_, _ = io.WriteString(writer, "<Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>")
					return
				}
				store.ServeHTTP(writer, request)
			}))
			t.Cleanup(server.Close)
			t.Setenv("AWS_REGION", "eu-west-1")
			t.Setenv("AWS_ACCESS_KEY_ID", "id")
			t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
			t.Setenv("AWS_SESSION_TOKEN", "")
			client, err := snapshots.NewS3(t.Context(), server.URL, true)
			require.NoError(t, err)
			require.ErrorContains(t, client.DeletePrefix(t.Context(), "s3://agent-snapshots/organization"), "AccessDenied")
			require.Empty(t, store.deleted)
		})
	}
}
