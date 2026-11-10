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
			assert.Equal(t, c.Mirrors, adminmirror.Mirrors(&c.Binding))
			if c.Mirrors {
				assert.Equal(t, c.Mirrored, adminmirror.MirroredSubjects(c.Binding.Subjects))
			} else {
				assert.Empty(t, c.Mirrored, "a binding that does not count now has no mirrored subjects")
			}
		})
	}
}
