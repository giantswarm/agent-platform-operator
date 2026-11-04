package controller

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestReadyCondition(t *testing.T) {
	missing := &notReadyError{reason: reasonSourceMissing, message: "the platform NetworkPolicy kagent/workers to copy is missing"}
	taken := &notReadyError{reason: reasonNameTaken, message: "the RoleBinding agents-x/kagent-flux is not the operator's"}
	failure := errors.New("create ServiceAccount agents-x/kagent-flux: forbidden")

	tests := []struct {
		name        string
		err         error
		wantStatus  metav1.ConditionStatus
		wantReason  string
		wantMessage string
		wantRetry   bool
	}{
		{
			name:       "no error is Ready",
			wantStatus: metav1.ConditionTrue, wantReason: reasonReconciled,
		},
		{
			name:       "causes of Ready=False are not retried and all are named",
			err:        errors.Join(missing, errors.Join(taken)),
			wantStatus: metav1.ConditionFalse, wantReason: reasonSourceMissing,
			wantMessage: missing.message + "; " + taken.message,
		},
		{
			name:       "a failure with a cause is retried and still names the cause",
			err:        errors.Join(missing, failure),
			wantStatus: metav1.ConditionFalse, wantReason: reasonReconcileFailed,
			wantMessage: failure.Error() + "; " + missing.message,
			wantRetry:   true,
		},
		{
			name:       "a wrapped cause is a failure, so a failure wrapped with it is not lost",
			err:        fmt.Errorf("mirror: %w", errors.Join(taken, failure)),
			wantStatus: metav1.ConditionFalse, wantReason: reasonReconcileFailed,
			wantMessage: "mirror: " + taken.message + "\n" + failure.Error(),
			wantRetry:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ready, err := readyCondition(tt.err, 3)
			assert.Equal(t, tt.wantStatus, ready.Status)
			assert.Equal(t, tt.wantReason, ready.Reason)
			assert.Equal(t, tt.wantMessage, ready.Message)
			assert.Equal(t, int64(3), ready.ObservedGeneration)
			assert.Equal(t, tt.wantRetry, err != nil, "retry: %v", err)
		})
	}
}
