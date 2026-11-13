package controller_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/giantswarm/agent-platform-operator/api/v1alpha1"
	"github.com/giantswarm/agent-platform-operator/internal/grpcwire"
	"github.com/giantswarm/agent-platform-operator/internal/grpcwire/grpcwiretest"
	"github.com/giantswarm/agent-platform-operator/internal/kagent"
)

const (
	// fluxNS is the namespace of the installation's Flux source, where an
	// organization's Flux Kustomization and Flux ServiceAccount live.
	fluxNS = "flux-giantswarm"
	// holdFinalizer keeps an object the erasure deletes until the erasure
	// observer has recorded its deletion.
	holdFinalizer = "test.giantswarm.io/hold"

	// The events of the erasure log.
	evKustomizationDeleted = "Kustomization deleted"
	evSessionsListed       = "sessions listed"
	evSessionsListedAll    = "sessions of every namespace listed"
	evSessionDeleted       = "session deleted"
	evNoActor              = "no actor"
	evSnapshotsDeleted     = "snapshots deleted"
	evNamespaceDeleted     = "namespace deleted"
	evFluxSADeleted        = "Flux ServiceAccount deleted"
	evWorkers              = "workers"

	// installationWide is the erasure log's key of a session listing without
	// a namespace.
	installationWide = "(installation)"

	// The packages kagent serves its SessionService in.
	pkgV1       = "kagent.api.v1"
	pkgV1alpha1 = "kagent.api.v1alpha1"
)

// erasureCases are the cases of a deleted organization's erasure, run in
// TestOrganizationEnvtest.
func erasureCases(t *testing.T, c *cluster) {
	t.Run("a deleted organization is erased in order", func(t *testing.T) {
		org := c.organizationWith(t, "org-erase", "erase", v1alpha1.AgentPlatformOrganizationSpec{
			Harnesses: []string{curatedHarness}, Workers: withReplicas(2),
		})
		ns := c.readyNamespace(t, org).Name
		org = c.get(t, org)
		assert.Contains(t, org.Finalizers, v1alpha1.Finalizer)
		prefix := org.Status.SnapshotPrefix
		// No ModelConfig: the live pool runs 0 workers.
		c.replicas(t, ns, 0)

		// The operator's own Flux objects (commit mode), held until the
		// observer records their deletion.
		kustomization, serviceAccount := c.holdFluxObjects(t, ns)
		// The UID label outside the Flux namespace names nothing the
		// operator created: it stays.
		elsewhere := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
			Name: "labelled-elsewhere", Namespace: "org-erase",
			Labels: map[string]string{v1alpha1.OrganizationUIDLabel: string(org.UID)},
		}}
		c.create(t, elsewhere)
		c.create(t, newObject(helmReleaseGVK, ns, "triage", nil, helmReleaseSpec))
		// An Agent written outside git, without the operator's label.
		c.create(t, newObject(agentGVK, ns, "by-hand", nil, agentSpec("By hand.", nil)))
		c.sessions.add(ns, "alice@example.com", "11111111-1111-1111-1111-111111111111")
		c.sessions.addOf(ns, "by-hand", "bob@example.com", "22222222-2222-2222-2222-222222222222")
		// A session of an Agent deleted before the erasure: no Agent of the
		// organization names it any more.
		c.sessions.addOf(ns, "retired", "carol@example.com", "55555555-5555-5555-5555-555555555555")
		other := c.organization(t, "org-erase-other", "kept")
		otherNS := c.readyNamespace(t, other).Name
		c.sessions.add(otherNS, "alice@example.com", "33333333-3333-3333-3333-333333333333")
		c.actors.linger[ns] = 2
		c.bucket.objects[prefix] = 4

		observed := c.observeErasure(t, org, ns, kustomization, serviceAccount)
		require.NoError(t, c.admin.Delete(t.Context(), org))
		c.gone(t, org)
		events := c.log.of(ns, prefix)
		t.Logf("erasure: %v", events)

		// The organization's sessions are all deleted, through kagent, its
		// Agents gone or not; the other organization's stay. kagent is asked
		// for the organization namespace's sessions only.
		assert.Empty(t, c.sessions.left(ns))
		assert.Equal(t, []string{"33333333-3333-3333-3333-333333333333"}, c.sessions.left(otherNS))
		assert.Empty(t, c.log.of(installationWide), "the erasure listed every namespace's sessions")
		assert.NotEmpty(t, all(events, evSessionsListed))
		assert.Zero(t, c.bucket.objects[prefix])
		assert.Empty(t, c.namespacesOf(t, org))
		require.NoError(t, c.admin.Get(t.Context(), client.ObjectKeyFromObject(elsewhere), &corev1.ServiceAccount{}), "a ServiceAccount labelled with the organization's UID outside the Flux namespace was deleted")
		assert.Equal(t, 0, *observed.workloadsAtSessions, "Kustomizations, HelmReleases or Agents of the organization were left when its sessions were listed")

		// Each step in order: the Kustomization, the sessions, no actor left,
		// the prefix twice, the namespace, the Flux ServiceAccount.
		order := []string{evKustomizationDeleted, evSessionsListed, evNoActor, evSnapshotsDeleted, evNamespaceDeleted, evFluxSADeleted}
		at := make([]int, len(order))
		for i, what := range order {
			at[i] = firstIndex(events, what)
			require.GreaterOrEqual(t, at[i], 0, "no %q in the erasure", what)
		}
		assert.True(t, slices.IsSorted(at), "the erasure steps ran out of order: %v at %v", order, at)
		assert.Less(t, lastIndex(events, evSessionDeleted), firstIndex(events, evNoActor), "an actor was counted gone before every session was deleted")
		// Twice, the delay apart, before the namespace goes.
		c.deletedTwice(t, events[:firstIndex(events, evNamespaceDeleted)])

		// The worker replica rule: the pool, at 0 without a ModelConfig,
		// runs its replicas once the organization is deleted until its
		// sessions are gone, and 0 after.
		workers := all(events, evWorkers)
		require.Len(t, workers, 3, "the pool's replicas: %v", workers)
		assert.Equal(t, []string{"0", "2", "0"}, []string{workers[0].detail, workers[1].detail, workers[2].detail})
		assert.Less(t, slices.Index(events, workers[1]), firstIndex(events, evSessionsListed))
		assert.Greater(t, slices.Index(events, workers[2]), firstIndex(events, evNoActor))

		t.Run("an organization re-created with the same parent and name starts empty", func(t *testing.T) {
			again := c.organizationWith(t, "org-erase", "erase", v1alpha1.AgentPlatformOrganizationSpec{Workers: withReplicas(2)})
			againNS := c.readyNamespace(t, again).Name
			again = c.get(t, again)
			assert.NotEqual(t, org.UID, again.UID)
			assert.NotEqual(t, ns, againNS)
			assert.NotEqual(t, prefix, again.Status.SnapshotPrefix)
			assert.Empty(t, c.sessions.left(againNS))
			assert.Zero(t, c.bucket.objects[again.Status.SnapshotPrefix])
			c.absent(t, helmReleaseGVK, againNS, "triage")
		})
	})

	t.Run("a failing erasure step keeps Ready=False Deleting with the step and is retried", func(t *testing.T) {
		org := c.organization(t, "org-erase-fail", "fails")
		ns := c.readyNamespace(t, org).Name
		prefix := c.get(t, org).Status.SnapshotPrefix
		c.bucket.failWith(prefix, errors.New("AccessDenied: the bucket refused the delete"))
		c.observeErasure(t, org, ns)
		require.NoError(t, c.admin.Delete(t.Context(), org))

		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			got := c.get(ct, org)
			assert.Equal(ct, v1alpha1.ErasureSnapshots, got.Status.ErasureStep)
			ready := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionReady)
			if assert.NotNil(ct, ready) {
				assert.Equal(ct, metav1.ConditionFalse, ready.Status)
				assert.Equal(ct, "Deleting", ready.Reason)
				assert.Contains(ct, ready.Message, "(Snapshots) failed")
				assert.Contains(ct, ready.Message, "AccessDenied")
			}
		}, timeout, tick)
		failures := len(all(c.log.of(prefix), evSnapshotsDeleted+" failed"))
		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			assert.Greater(ct, len(all(c.log.of(prefix), evSnapshotsDeleted+" failed")), failures, "the failing step is not retried")
		}, timeout, tick)
		assert.NotEmpty(t, c.namespacesOf(t, org), "the namespace went while the snapshots were not erased")

		c.bucket.failWith(prefix, nil)
		c.gone(t, org)
		c.deletedTwice(t, c.log.of(prefix))
	})

	t.Run("the erasure follows kagent from kagent.api.v1alpha1 to kagent.api.v1", func(t *testing.T) {
		// The operator's client last answered on kagent.api.v1alpha1.
		c.sessions.serving(pkgV1)
		t.Cleanup(func() { c.sessions.serving(pkgV1alpha1) })
		org := c.organization(t, "org-erase-v1", "upgraded")
		ns := c.readyNamespace(t, org).Name
		c.sessions.addOf(ns, "retired", "dave@example.com", "66666666-6666-6666-6666-666666666666")
		other := c.organization(t, "org-erase-v1-other", "kept")
		otherNS := c.readyNamespace(t, other).Name
		c.sessions.add(otherNS, "dave@example.com", "77777777-7777-7777-7777-777777777777")
		c.observeErasure(t, org, ns)

		require.NoError(t, c.admin.Delete(t.Context(), org))
		c.gone(t, org)

		// kagent.api.v1 has no namespace filter: every namespace's sessions
		// are listed, and only the organization's deleted.
		assert.Empty(t, c.sessions.left(ns))
		assert.Equal(t, []string{"77777777-7777-7777-7777-777777777777"}, c.sessions.left(otherNS))
	})

	t.Run("deleting the parent namespace erases its organizations", func(t *testing.T) {
		org := c.organization(t, "org-parent-gone", "orphan")
		ns := c.readyNamespace(t, org).Name
		prefix := c.get(t, org).Status.SnapshotPrefix
		c.create(t, newObject(helmReleaseGVK, ns, "triage", nil, helmReleaseSpec))
		c.sessions.add(ns, "carol@example.com", "44444444-4444-4444-4444-444444444444")
		c.bucket.objects[prefix] = 1
		c.observeErasure(t, org, ns)

		parent := &corev1.Namespace{}
		require.NoError(t, c.admin.Get(t.Context(), client.ObjectKey{Name: org.Namespace}, parent))
		require.NoError(t, c.admin.Delete(t.Context(), parent))
		// envtest runs no namespace controller: delete what it would, the
		// objects of the terminating parent.
		c.deleteNamespaceContent(t, org.Namespace)
		c.gone(t, org)

		assert.Empty(t, c.sessions.left(ns))
		assert.Zero(t, c.bucket.objects[prefix])
		c.deletedTwice(t, c.log.of(prefix))
		assert.Empty(t, c.namespacesOf(t, org))
		c.finalizeNamespace(t, org.Namespace)
	})
}

// ---- fakes ------------------------------------------------------------------

// event is one entry of the erasure log.
type event struct {
	what, key, detail string
	at                time.Time
}

func (e event) String() string { return e.what + " " + e.detail }

// eventLog is what the erasure did, in order, through the fakes and as the
// observers saw it.
type eventLog struct {
	mu     sync.Mutex
	events []event
}

func (l *eventLog) add(what, key, detail string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event{what: what, key: key, detail: detail, at: time.Now()})
}

// of are the events of keys: an organization's namespace and snapshot prefix.
func (l *eventLog) of(keys ...string) []event {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []event
	for _, e := range l.events {
		if slices.Contains(keys, e.key) {
			out = append(out, e)
		}
	}
	return out
}

func firstIndex(events []event, what string) int {
	return slices.IndexFunc(events, func(e event) bool { return e.what == what })
}

func lastIndex(events []event, what string) int {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].what == what {
			return i
		}
	}
	return -1
}

func all(events []event, what string) []event {
	var out []event
	for _, e := range events {
		if e.what == what {
			out = append(out, e)
		}
	}
	return out
}

// fakeSession is a kagent session whose pinned revision is of the Agent
// agent in namespace.
type fakeSession struct {
	namespace, agent, creator string
	deleted                   bool
	// gone: kagent deletes a session asynchronously, so a deleted session is
	// still listed once.
	gone bool
}

// fakeSessions stands in for kagent's SessionService as the erasure needs it,
// served over gRPC to the operator's client: it deletes any creator's session
// for the operator. kagent's session service does that only for its control
// plane today (see kagent.Sessions.Delete).
type fakeSessions struct {
	log  *eventLog
	mu   sync.Mutex
	byID map[string]*fakeSession
	// pkg is the one package the SessionService is served in:
	// kagent.api.v1alpha1, the kagent line agent-platform pins, with the
	// namespace filter; kagent.api.v1, without it.
	pkg string
	// onList runs at each listing, before it, with the namespace asked for
	// (empty: every namespace).
	onList func(namespace string)
	// unavailable: kagent answers every listing Unavailable.
	unavailable bool
}

// failing makes every listing fail (Unavailable) while down.
func (f *fakeSessions) failing(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unavailable = down
}

func (f *fakeSessions) add(namespace, creator, id string) {
	f.addOf(namespace, "triage", creator, id)
}

func (f *fakeSessions) addOf(namespace, agent, creator, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byID[id] = &fakeSession{namespace: namespace, agent: agent, creator: creator}
}

// left are the IDs of the sessions of namespace not deleted.
func (f *fakeSessions) left(namespace string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []string
	for id, s := range f.byID {
		if s.namespace == namespace && !s.deleted {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// serve serves f over gRPC and returns the operator's kagent client of it.
func (f *fakeSessions) serve(t *testing.T) *kagent.Sessions {
	_, conn := grpcwiretest.Start(t, "operator-token", f.handle)
	return kagent.New(conn)
}

// serving makes kagent serve its SessionService in pkg only.
func (f *fakeSessions) serving(pkg string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pkg = pkg
}

// handle answers ListSessions and DeleteSession, with the field numbers of
// kagent's proto/kagent/api/{v1,v1alpha1}/sessions.proto.
func (f *fakeSessions) handle(call grpcwiretest.Call) ([]byte, error) {
	fields, err := grpcwire.Fields(call.Request)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	f.mu.Lock()
	pkg, unavailable := f.pkg, f.unavailable
	f.mu.Unlock()
	switch call.Method {
	case "/" + pkg + ".SessionService/ListSessions":
		if unavailable {
			return nil, status.Error(codes.Unavailable, "kagent is down")
		}
		if pkg == pkgV1 {
			// No namespace filter: an unknown field is ignored.
			return f.list(pkg, ""), nil
		}
		// ListSessionsRequest: namespace = 7.
		return f.list(pkg, string(grpcwire.First(fields, 7))), nil
	case "/" + pkg + ".SessionService/DeleteSession":
		// DeleteSessionRequest: session_id = 1.
		f.delete(string(grpcwire.First(fields, 1)))
		return nil, nil
	}
	return nil, status.Errorf(codes.Unimplemented, "%s", call.Method)
}

// list is a ListSessionsResponse, on one page, of the sessions of namespace
// (empty: every namespace). kagent matches the namespace of each session's
// pinned revision, so a session whose Agent is gone is listed.
func (f *fakeSessions) list(pkg, namespace string) []byte {
	if hook := f.onList; hook != nil {
		hook(namespace)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if namespace == "" {
		f.log.add(evSessionsListedAll, installationWide, "")
	} else {
		f.log.add(evSessionsListed, namespace, "")
	}
	resp := grpcwire.Message(nil)
	for id, s := range f.byID {
		if (namespace != "" && s.namespace != namespace) || s.gone {
			continue
		}
		// Session{id = 1, agent = 15}: in kagent.api.v1alpha1 the
		// ResourceReference{namespace = 1, name = 2}, in kagent.api.v1 an
		// AgentProvenance{ref = 1, uid = 2} holding it.
		agent := grpcwire.Message(nil).String(1, s.namespace).String(2, s.agent)
		if pkg == pkgV1 {
			agent = grpcwire.Message(nil).Message(1, agent).String(2, "uid-"+s.agent)
		}
		resp = resp.Message(1, grpcwire.Message(nil).String(1, id).Message(15, agent))
		s.gone = s.deleted
	}
	return resp
}

func (f *fakeSessions) delete(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.byID[id]
	if !ok {
		return
	}
	s.deleted = true
	f.log.add(evSessionDeleted, s.namespace, id)
}

// fakeActors stands in for Substrate's actors: an actor per session left,
// and linger[atespace] more after the last session went, since an actor
// outlives its session until kagent has suspended it.
type fakeActors struct {
	log      *eventLog
	sessions *fakeSessions
	mu       sync.Mutex
	linger   map[string]int
}

func (f *fakeActors) Count(_ context.Context, atespace string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(f.sessions.left(atespace))
	if n == 0 && f.linger[atespace] > 0 {
		f.linger[atespace]--
		n = 1
	}
	if n == 0 {
		f.log.add(evNoActor, atespace, "")
	}
	return n, nil
}

// fakeBucket stands in for the snapshot bucket: a count of objects by
// prefix.
type fakeBucket struct {
	log     *eventLog
	mu      sync.Mutex
	objects map[string]int
	fail    map[string]error
}

func (f *fakeBucket) failWith(prefix string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail[prefix] = err
}

func (f *fakeBucket) DeletePrefix(_ context.Context, prefix string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail[prefix]; err != nil {
		f.log.add(evSnapshotsDeleted+" failed", prefix, err.Error())
		return err
	}
	f.objects[prefix] = 0
	f.log.add(evSnapshotsDeleted, prefix, "")
	return nil
}

// ---- helpers ----------------------------------------------------------------

// observed is what the erasure observer saw.
type observed struct {
	// workloadsAtSessions is the number of Kustomizations, HelmReleases and
	// Agents of the organization at its first session listing.
	workloadsAtSessions *int
}

// observeErasure records in the erasure log what the erasure of org deletes
// in the Kubernetes API, until the test ends, and stands in for the
// controllers envtest does not run: it removes the hold finalizer from each
// of held once its deletion is recorded, and finalizes the organization
// namespace. It also records each change of the WorkerPool copy's replicas,
// and counts the workloads left at the first session listing.
func (c *cluster) observeErasure(t *testing.T, org *v1alpha1.AgentPlatformOrganization, ns string, held ...client.Object) *observed {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	seen := &observed{}
	var once sync.Once
	c.sessions.onList = func(namespace string) {
		if namespace != ns && namespace != "" {
			return
		}
		once.Do(func() {
			n := c.count(kustomizationGVK, fluxNS, client.MatchingLabels{v1alpha1.OrganizationUIDLabel: string(org.UID)}) +
				c.count(helmReleaseGVK, ns, nil) + c.count(agentGVK, ns, nil)
			seen.workloadsAtSessions = &n
		})
	}
	t.Cleanup(func() {
		cancel()
		<-done
		c.sessions.onList = nil
	})
	go func() {
		defer close(done)
		recorded := map[string]bool{}
		replicas := int64(-1)
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
			for _, o := range held {
				what := evKustomizationDeleted
				if _, ok := o.(*corev1.ServiceAccount); ok {
					what = evFluxSADeleted
				}
				if !recorded[what] && c.deleting(ctx, o) {
					c.log.add(what, ns, o.GetName())
					recorded[what] = true
					c.release(ctx, o)
				}
			}
			namespace := &corev1.Namespace{}
			if err := c.admin.Get(ctx, client.ObjectKey{Name: ns}, namespace); err == nil && namespace.DeletionTimestamp != nil {
				if !recorded[evNamespaceDeleted] {
					c.log.add(evNamespaceDeleted, ns, "")
					recorded[evNamespaceDeleted] = true
				}
				namespace.Spec.Finalizers = nil
				_ = c.admin.SubResource("finalize").Update(ctx, namespace)
			}
			pool := &unstructured.Unstructured{}
			pool.SetGroupVersionKind(poolGVK)
			if err := c.admin.Get(ctx, client.ObjectKey{Namespace: ns, Name: curatedPool}, pool); err == nil {
				if n, _, _ := unstructured.NestedInt64(pool.Object, "spec", "replicas"); n != replicas {
					c.log.add(evWorkers, ns, fmt.Sprint(n))
					replicas = n
				}
			}
		}
	}()
	return seen
}

// holdFluxObjects waits for the Kustomization and the Flux ServiceAccount the
// operator keeps in fluxNS for the organization of namespace ns, and holds
// both with holdFinalizer.
func (c *cluster) holdFluxObjects(t *testing.T, ns string) (client.Object, client.Object) {
	t.Helper()
	kustomization := c.eventually(t, kustomizationGVK, fluxNS, ns, nil)
	serviceAccount := &corev1.ServiceAccount{}
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		assert.NoError(ct, c.admin.Get(t.Context(), client.ObjectKey{Namespace: fluxNS, Name: ns}, serviceAccount))
	}, timeout, tick)
	for _, o := range []client.Object{kustomization, serviceAccount} {
		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			got := o.DeepCopyObject().(client.Object)
			if assert.NoError(ct, c.admin.Get(t.Context(), client.ObjectKeyFromObject(o), got)) {
				got.SetFinalizers(append(got.GetFinalizers(), holdFinalizer))
				assert.NoError(ct, c.admin.Update(t.Context(), got))
			}
		}, timeout, tick)
	}
	return kustomization, serviceAccount
}

func (c *cluster) create(t *testing.T, obj client.Object) {
	t.Helper()
	require.NoError(t, c.admin.Create(t.Context(), obj), "create %s", obj.GetName())
}

// deleting reports whether o carries a deletion timestamp.
func (c *cluster) deleting(ctx context.Context, o client.Object) bool {
	got := o.DeepCopyObject().(client.Object)
	if err := c.admin.Get(ctx, client.ObjectKeyFromObject(o), got); err != nil {
		return false
	}
	return got.GetDeletionTimestamp() != nil
}

// release removes holdFinalizer from o.
func (c *cluster) release(ctx context.Context, o client.Object) {
	got := o.DeepCopyObject().(client.Object)
	if err := c.admin.Get(ctx, client.ObjectKeyFromObject(o), got); err != nil {
		return
	}
	got.SetFinalizers(slices.DeleteFunc(got.GetFinalizers(), func(f string) bool { return f == holdFinalizer }))
	_ = c.admin.Update(ctx, got)
}

// count is the number of objects of gvk in ns (every namespace when empty)
// matching selector.
func (c *cluster) count(gvk schema.GroupVersionKind, ns string, selector client.MatchingLabels) int {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
	if err := c.admin.List(context.Background(), list, client.InNamespace(ns), selector); err != nil {
		return -1
	}
	return len(list.Items)
}

// deletedTwice asserts that events delete the snapshot prefix at least
// twice, the last time at least the snapshot delay after the first. A retry
// after a lost status write, or a reconcile of a stale cached organization,
// may delete it once more: deleting an empty prefix again is harmless.
func (c *cluster) deletedTwice(t *testing.T, events []event) {
	t.Helper()
	deletions := all(events, evSnapshotsDeleted)
	require.GreaterOrEqual(t, len(deletions), 2, "the snapshot prefix was not deleted twice")
	assert.GreaterOrEqual(t, deletions[len(deletions)-1].at.Sub(deletions[0].at), snapshotDelay)
}

// gone waits until org no longer exists.
func (c *cluster) gone(t *testing.T, org *v1alpha1.AgentPlatformOrganization) {
	t.Helper()
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		err := c.admin.Get(context.Background(), client.ObjectKeyFromObject(org), &v1alpha1.AgentPlatformOrganization{})
		assert.True(ct, apierrors.IsNotFound(err), "the organization is still there: %v", err)
	}, timeout, tick)
}

// deleteNamespaceContent deletes the AgentPlatformOrganizations in ns, as the
// namespace controller does in a terminating namespace.
func (c *cluster) deleteNamespaceContent(t *testing.T, ns string) {
	t.Helper()
	require.NoError(t, c.admin.DeleteAllOf(t.Context(), &v1alpha1.AgentPlatformOrganization{}, client.InNamespace(ns)))
}

// finalizeNamespace ends a terminating namespace, as the namespace controller
// does once it is empty.
func (c *cluster) finalizeNamespace(t *testing.T, name string) {
	t.Helper()
	ns := &corev1.Namespace{}
	require.NoError(t, c.admin.Get(t.Context(), client.ObjectKey{Name: name}, ns))
	ns.Spec.Finalizers = nil
	require.NoError(t, c.admin.SubResource("finalize").Update(t.Context(), ns))
}
