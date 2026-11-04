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

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
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
		var req struct {
			Objects []struct{ Key string } `xml:"Object"`
		}
		if err := xml.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_, _ = fmt.Fprint(w, `<DeleteResult>`)
		for _, o := range req.Objects {
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
	return snapshots.NewS3WithClient(s3.New(s3.Options{
		BaseEndpoint: aws.String(srv.URL), UsePathStyle: true, Region: "eu-west-1",
		Credentials: credentials.NewStaticCredentialsProvider("id", "secret", ""),
	}))
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
