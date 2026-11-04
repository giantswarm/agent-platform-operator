package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/giantswarm/agent-platform-operator/api/v1alpha1"
)

const reasonDeleting = "Deleting"

// Sessions is kagent's session API, as the operator's identity.
type Sessions interface {
	// List returns the IDs of every creator's sessions of the Agents in
	// namespace.
	List(ctx context.Context, namespace string) ([]string, error)
	// Delete deletes one session; one already gone is no error.
	Delete(ctx context.Context, id string) error
}

// Actors is Substrate's actor listing.
type Actors interface {
	// Count is the number of actors in atespace (the organization
	// namespace's name), whatever their state.
	Count(ctx context.Context, atespace string) (int, error)
}

// Snapshots is the worker snapshot bucket.
type Snapshots interface {
	// DeletePrefix deletes every object under prefix, s3://<bucket>/<uid>.
	DeletePrefix(ctx context.Context, prefix string) error
}

// ErasureConfig is how the reconciler erases a deleted organization.
type ErasureConfig struct {
	// Sessions, Actors and Snapshots erase what the organization left in
	// kagent, Substrate and the snapshot bucket; a nil one is not in this
	// installation, and its part of a step is skipped.
	Sessions  Sessions
	Actors    Actors
	Snapshots Snapshots
	// SnapshotDelay is how long after the first deletion of the snapshot
	// prefix the operator deletes it again: at least Substrate's actor
	// workflow deadline, so an upload that started before the last actor
	// went has ended.
	SnapshotDelay time.Duration
	// Poll is how often a step that waits for something to be gone looks
	// again.
	Poll time.Duration
	// HelmRelease and Agent are the served kinds the first step deletes after
	// the organization's Kustomization (FluxConfig.Kustomization); an empty
	// or unserved one has nothing to delete.
	HelmRelease schema.GroupVersionKind
	Agent       schema.GroupVersionKind
}

// erasureSteps are the steps of the erasure, in order; the finalizer's
// removal is the last.
var erasureSteps = []string{
	v1alpha1.ErasureWorkloads,
	v1alpha1.ErasureSessions,
	v1alpha1.ErasureSnapshots,
	v1alpha1.ErasureNamespace,
	v1alpha1.ErasureOutsideObjects,
}

// waiting is a step that is not done yet and looks again after after.
type waiting struct {
	message string
	after   time.Duration
}

// sessionsGone reports whether the erasure has passed the Sessions step: the
// organization has no session and no actor left.
func sessionsGone(step string) bool {
	return slices.Index(erasureSteps, step) > slices.Index(erasureSteps, v1alpha1.ErasureSessions)
}

// erase runs the erasure of a deleted organization from the step its status
// records, and removes the finalizer once every step is done. A failing step
// is retried with the controller's backoff; a step that waits looks again
// after the poll interval. Ready stays False with reason Deleting, and its
// message names the step.
func (r *OrganizationReconciler) erase(ctx context.Context, org *v1alpha1.AgentPlatformOrganization) (reconcile.Result, error) {
	// The progress is in the status: read it from the API server, so a
	// reconcile of a stale cached copy never runs a step it has passed (the
	// workers back up after the sessions went, the prefix deleted again).
	fresh := &v1alpha1.AgentPlatformOrganization{}
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(org), fresh); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	org = fresh
	if !controllerutil.ContainsFinalizer(org, v1alpha1.Finalizer) {
		return reconcile.Result{}, nil
	}
	status := org.Status.DeepCopy()
	start := max(slices.Index(erasureSteps, status.ErasureStep), 0)
	var (
		wait *waiting
		err  error
	)
	for i := start; i < len(erasureSteps) && wait == nil && err == nil; i++ {
		status.ErasureStep = erasureSteps[i]
		wait, err = r.eraseStep(ctx, org, status)
	}

	step := slices.Index(erasureSteps, status.ErasureStep) + 1
	message := fmt.Sprintf("erasure step %d of %d (%s)", step, len(erasureSteps)+1, status.ErasureStep)
	switch {
	case err != nil:
		message += " failed: " + err.Error()
	case wait != nil:
		message += ": " + wait.message
	default:
		message = "erased; removing the finalizer"
	}
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type: v1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: reasonDeleting,
		Message: message, ObservedGeneration: org.Generation,
	})
	if !equality.Semantic.DeepEqual(&org.Status, status) {
		org.Status = *status
		if uerr := r.Client.Status().Update(ctx, org, client.FieldOwner(fieldManager)); uerr != nil {
			return reconcile.Result{}, errors.Join(err, fmt.Errorf("update status: %w", uerr))
		}
	}
	switch {
	case err != nil:
		return reconcile.Result{}, err
	case wait != nil:
		return reconcile.Result{RequeueAfter: wait.after}, nil
	}
	patch := client.MergeFromWithOptions(org.DeepCopy(), client.MergeFromWithOptimisticLock{})
	controllerutil.RemoveFinalizer(org, v1alpha1.Finalizer)
	if err := r.Client.Patch(ctx, org, patch); client.IgnoreNotFound(err) != nil {
		return reconcile.Result{}, fmt.Errorf("remove the finalizer: %w", err)
	}
	logf.FromContext(ctx).Info("organization erased")
	return reconcile.Result{}, nil
}

// eraseStep runs the step status.ErasureStep names.
func (r *OrganizationReconciler) eraseStep(ctx context.Context, org *v1alpha1.AgentPlatformOrganization, status *v1alpha1.AgentPlatformOrganizationStatus) (*waiting, error) {
	ns, err := r.erasedNamespace(ctx, org)
	if err != nil {
		return nil, err
	}
	switch status.ErasureStep {
	case v1alpha1.ErasureWorkloads:
		if err := r.scaleWorkers(ctx, org, status, ns); err != nil {
			return nil, err
		}
		return r.eraseWorkloads(ctx, org, ns)
	case v1alpha1.ErasureSessions:
		if err := r.scaleWorkers(ctx, org, status, ns); err != nil {
			return nil, err
		}
		return r.eraseSessions(ctx, ns)
	case v1alpha1.ErasureSnapshots:
		if err := r.scaleWorkers(ctx, org, status, ns); err != nil {
			return nil, err
		}
		return r.eraseSnapshots(ctx, org, status)
	case v1alpha1.ErasureNamespace:
		return r.eraseNamespace(ctx, org)
	case v1alpha1.ErasureOutsideObjects:
		return r.eraseOutsideObjects(ctx, org)
	}
	return nil, fmt.Errorf("unknown erasure step %q", status.ErasureStep)
}

// erasedNamespace is the name of the organization namespace: the one
// labelled with the organization's UID, else the one the status recorded
// (empty when none was ever created). kagent and Substrate key the
// organization's sessions and actors by that name, also once it is gone.
func (r *OrganizationReconciler) erasedNamespace(ctx context.Context, org *v1alpha1.AgentPlatformOrganization) (string, error) {
	list := &corev1.NamespaceList{}
	if err := r.APIReader.List(ctx, list, client.MatchingLabels{v1alpha1.OrganizationUIDLabel: string(org.UID)}); err != nil {
		return "", fmt.Errorf("list the namespaces labelled %s=%s: %w", v1alpha1.OrganizationUIDLabel, org.UID, err)
	}
	if len(list.Items) > 0 {
		return list.Items[0].Name, nil
	}
	return org.Status.Namespace, nil
}

// scaleWorkers keeps the organization's WorkerPool copy at the worker replica
// rule's count for a deleted organization: its replicas until its sessions
// are gone, so kagent can suspend its actors, and 0 after.
func (r *OrganizationReconciler) scaleWorkers(ctx context.Context, org *v1alpha1.AgentPlatformOrganization, status *v1alpha1.AgentPlatformOrganizationStatus, ns string) error {
	c := r.Config.Curated
	if !c.Enabled() || ns == "" {
		return nil
	}
	pool := object(c.pool())
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: c.WorkerPool}, pool)
	if apierrors.IsNotFound(err) || (err == nil && (pool.GetLabels()[ObjectLabel] != ScopeCurated || pool.GetDeletionTimestamp() != nil)) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get WorkerPool %s/%s: %w", ns, c.WorkerPool, err)
	}
	erasing := org.DeepCopy()
	erasing.Status = *status
	want := workerReplicas(erasing, true)
	if have, _, _ := unstructured.NestedInt64(pool.Object, "spec", "replicas"); have == want {
		return nil
	}
	if err := unstructured.SetNestedField(pool.Object, want, "spec", "replicas"); err != nil {
		return err
	}
	if err := r.Client.Update(ctx, pool, client.FieldOwner(fieldManager)); err != nil {
		return fmt.Errorf("scale WorkerPool %s/%s to %d: %w", ns, c.WorkerPool, want, err)
	}
	return nil
}

// eraseWorkloads deletes the organization's Flux Kustomization and waits
// until it is gone, then deletes the HelmReleases left in the namespace (all
// of them where there is no Kustomization) and its Agents, and waits until
// they are gone.
func (r *OrganizationReconciler) eraseWorkloads(ctx context.Context, org *v1alpha1.AgentPlatformOrganization, ns string) (*waiting, error) {
	e, f := r.Config.Erasure, r.Config.Flux
	// The organization's Flux objects are searched in the Flux namespace
	// only: the UID label elsewhere names nothing the operator created.
	if f.Namespace != "" {
		left, err := r.deleteAll(ctx, f.Kustomization, f.Namespace, fluxLabels(org))
		if err != nil || len(left) > 0 {
			return r.waitFor(left), err
		}
	}
	if ns == "" {
		return nil, nil
	}
	for _, gvk := range []schema.GroupVersionKind{e.HelmRelease, e.Agent} {
		left, err := r.deleteAll(ctx, gvk, ns, nil)
		if err != nil || len(left) > 0 {
			return r.waitFor(left), err
		}
	}
	return nil, nil
}

// eraseSessions deletes every session of the organization's Agents through
// kagent, until none is left, then waits until Substrate holds no actor of
// the organization: once none is left, no snapshot upload starts.
func (r *OrganizationReconciler) eraseSessions(ctx context.Context, ns string) (*waiting, error) {
	e := r.Config.Erasure
	if ns == "" {
		return nil, nil
	}
	if e.Sessions != nil {
		ids, err := e.Sessions.List(ctx, ns)
		if err != nil {
			return nil, err
		}
		var errs []error
		for _, id := range ids {
			errs = append(errs, e.Sessions.Delete(ctx, id))
		}
		if err := errors.Join(errs...); err != nil {
			return nil, err
		}
		if len(ids) > 0 {
			// kagent deletes a session asynchronously: list again.
			return &waiting{message: fmt.Sprintf("%d sessions of %s deleted; waiting until none is left", len(ids), ns), after: e.Poll}, nil
		}
	}
	if e.Actors != nil {
		n, err := e.Actors.Count(ctx, ns)
		if err != nil {
			return nil, err
		}
		if n > 0 {
			return &waiting{message: fmt.Sprintf("%d Substrate actors left in %s", n, ns), after: e.Poll}, nil
		}
	}
	return nil, nil
}

// eraseSnapshots deletes the organization's snapshot prefix, records when,
// and deletes it once more after the snapshot delay.
func (r *OrganizationReconciler) eraseSnapshots(ctx context.Context, org *v1alpha1.AgentPlatformOrganization, status *v1alpha1.AgentPlatformOrganizationStatus) (*waiting, error) {
	e := r.Config.Erasure
	// The prefix the status recorded last.
	prefix := org.Status.SnapshotPrefix
	if prefix == "" {
		prefix = r.snapshotPrefix(org)
	}
	if e.Snapshots == nil || prefix == "" {
		return nil, nil
	}
	if status.SnapshotsErasedAt == nil {
		if err := e.Snapshots.DeletePrefix(ctx, prefix); err != nil {
			return nil, err
		}
		now := metav1.NowMicro()
		status.SnapshotsErasedAt = &now
	}
	if left := time.Until(status.SnapshotsErasedAt.Add(e.SnapshotDelay)); left > 0 {
		return &waiting{
			message: fmt.Sprintf("%s emptied; emptying it again at %s", prefix, status.SnapshotsErasedAt.Add(e.SnapshotDelay).UTC().Format(time.RFC3339)),
			after:   left,
		}, nil
	}
	return nil, e.Snapshots.DeletePrefix(ctx, prefix)
}

// eraseNamespace deletes the organization namespace and waits until it is
// gone.
func (r *OrganizationReconciler) eraseNamespace(ctx context.Context, org *v1alpha1.AgentPlatformOrganization) (*waiting, error) {
	list := &corev1.NamespaceList{}
	if err := r.APIReader.List(ctx, list, client.MatchingLabels{v1alpha1.OrganizationUIDLabel: string(org.UID)}); err != nil {
		return nil, fmt.Errorf("list the namespaces labelled %s=%s: %w", v1alpha1.OrganizationUIDLabel, org.UID, err)
	}
	var left []string
	for i := range list.Items {
		ns := &list.Items[i]
		left = append(left, "Namespace "+ns.Name)
		if ns.DeletionTimestamp != nil {
			continue
		}
		if err := r.Client.Delete(ctx, ns); client.IgnoreNotFound(err) != nil {
			return nil, fmt.Errorf("delete Namespace %s: %w", ns.Name, err)
		}
		logf.FromContext(ctx).Info("organization namespace deleted", "namespace", ns.Name)
	}
	return r.waitFor(left), nil
}

// eraseOutsideObjects deletes the organization's Flux ServiceAccount beside
// the installation's Flux source and waits until it is gone.
func (r *OrganizationReconciler) eraseOutsideObjects(ctx context.Context, org *v1alpha1.AgentPlatformOrganization) (*waiting, error) {
	ns := r.Config.Flux.Namespace
	if ns == "" {
		return nil, nil
	}
	list := &corev1.ServiceAccountList{}
	if err := r.APIReader.List(ctx, list, client.InNamespace(ns), fluxLabels(org)); err != nil {
		return nil, fmt.Errorf("list the organization's Flux ServiceAccounts: %w", err)
	}
	var left []string
	for i := range list.Items {
		sa := &list.Items[i]
		left = append(left, "ServiceAccount "+sa.Namespace+"/"+sa.Name)
		if sa.DeletionTimestamp != nil {
			continue
		}
		if err := r.Client.Delete(ctx, sa); client.IgnoreNotFound(err) != nil {
			return nil, fmt.Errorf("delete ServiceAccount %s/%s: %w", sa.Namespace, sa.Name, err)
		}
		logf.FromContext(ctx).Info("organization object deleted", "namespace", sa.Namespace, "kind", "ServiceAccount", "name", sa.Name)
	}
	return r.waitFor(left), nil
}

// deleteAll deletes every object of gvk in ns (every namespace when ns is
// empty) that matches selector, and returns those still there. A kind the API
// server does not serve has none.
func (r *OrganizationReconciler) deleteAll(ctx context.Context, gvk schema.GroupVersionKind, ns string, selector client.MatchingLabels) ([]string, error) {
	if gvk.Empty() {
		return nil, nil
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
	opts := []client.ListOption{client.InNamespace(ns)}
	if selector != nil {
		opts = append(opts, selector)
	}
	if err := r.Client.List(ctx, list, opts...); meta.IsNoMatchError(err) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("list %ss: %w", gvk.Kind, err)
	}
	var left []string
	for i := range list.Items {
		o := &list.Items[i]
		left = append(left, gvk.Kind+" "+o.GetNamespace()+"/"+o.GetName())
		if o.GetDeletionTimestamp() != nil {
			continue
		}
		if err := r.Client.Delete(ctx, o, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
			return nil, fmt.Errorf("delete %s %s/%s: %w", gvk.Kind, o.GetNamespace(), o.GetName(), err)
		}
		logf.FromContext(ctx).Info("organization object deleted", "namespace", o.GetNamespace(), "kind", gvk.Kind, "name", o.GetName())
	}
	return left, nil
}

// waitFor waits for the objects named in left to be gone; none left waits for
// nothing.
func (r *OrganizationReconciler) waitFor(left []string) *waiting {
	if len(left) == 0 {
		return nil
	}
	return &waiting{message: "waiting until these are gone: " + strings.Join(left, ", "), after: r.Config.Erasure.Poll}
}

// fluxLabels select, in the Flux namespace, the organization's Flux
// Kustomization and Flux ServiceAccount: those labelled with its UID.
func fluxLabels(org *v1alpha1.AgentPlatformOrganization) client.MatchingLabels {
	return client.MatchingLabels{v1alpha1.OrganizationUIDLabel: string(org.UID)}
}
