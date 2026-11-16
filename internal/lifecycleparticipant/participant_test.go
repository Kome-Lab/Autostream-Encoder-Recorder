package lifecycleparticipant

import "testing"

func TestIUWS3AdmissionCutAndRestart(t *testing.T) {
	if _, err := NewSyntheticParticipant(SyntheticConfig{}); err == nil {
		t.Fatal("missing trusted state and limits accepted")
	}
	t.Run("cut_barrier_restart_and_partial_restore", testCutRestart)
}
