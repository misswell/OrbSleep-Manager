package failpoint

import "testing"

func TestInertWithoutEnv(t *testing.T) {
	t.Setenv("ORBSLEEP_FAILPOINT", "")
	Hit("after_compose_write")
}
