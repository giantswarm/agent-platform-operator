package adminmirror_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/giantswarm/agent-platform-operator/pkg/adminmirror"
)

func TestCases(t *testing.T) {
	for _, c := range adminmirror.Cases() {
		t.Run(c.Binding.Name, func(t *testing.T) {
			assert.Equal(t, c.IsMirroredAdmin, adminmirror.IsMirroredAdmin(&c.Binding))
			if c.IsMirroredAdmin {
				assert.Equal(t, c.Mirrored, adminmirror.MirroredSubjects(c.Binding.Subjects))
			} else {
				assert.Empty(t, c.Mirrored, "a binding that is not mirrored has no mirrored subjects")
			}
		})
	}
}
