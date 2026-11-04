package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// ObjectQuotaName is the ResourceQuota on object counts the reconciler keeps
// in every organization namespace, so one organization cannot flood the API
// server with objects.
const ObjectQuotaName = "agent-platform-organization-objects"

// ensureObjectQuota keeps the ResourceQuota ObjectQuotaName in ns with
// Config.ObjectQuota as its hard limits, and removes its own when the limits
// are empty. A ResourceQuota of that name the reconciler did not write makes
// the organization not Ready.
func (r *OrganizationReconciler) ensureObjectQuota(ctx context.Context, ns string) error {
	want := corev1.ResourceQuotaSpec{Hard: r.Config.ObjectQuota}
	existing := &corev1.ResourceQuota{}
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: ObjectQuotaName}, existing)
	switch {
	case apierrors.IsNotFound(err):
		if len(want.Hard) == 0 {
			return nil
		}
		return r.create(ctx, &corev1.ResourceQuota{
			ObjectMeta: metav1.ObjectMeta{Name: ObjectQuotaName, Namespace: ns, Labels: ownLabels(ScopeNamespace)},
			Spec:       want,
		}, "ResourceQuota")
	case err != nil:
		return fmt.Errorf("get ResourceQuota %s/%s: %w", ns, ObjectQuotaName, err)
	case existing.Labels[ObjectLabel] != ScopeNamespace:
		return &notReadyError{reason: reasonNameTaken, message: fmt.Sprintf("the ResourceQuota %s/%s is not the operator's, so the operator cannot write it", ns, ObjectQuotaName)}
	case len(want.Hard) == 0:
		if err := r.Client.Delete(ctx, existing); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete ResourceQuota %s/%s: %w", ns, ObjectQuotaName, err)
		}
		logf.FromContext(ctx).Info("organization object deleted", "namespace", ns, "kind", "ResourceQuota", "name", ObjectQuotaName)
		return nil
	case equality.Semantic.DeepEqual(existing.Spec, want):
		return nil
	}
	existing.Spec = want
	if err := r.Client.Update(ctx, existing, client.FieldOwner(fieldManager)); err != nil {
		return fmt.Errorf("update ResourceQuota %s/%s: %w", ns, ObjectQuotaName, err)
	}
	logf.FromContext(ctx).Info("organization object updated", "namespace", ns, "kind", "ResourceQuota", "name", ObjectQuotaName)
	return nil
}
