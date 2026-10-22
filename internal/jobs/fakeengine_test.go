package jobs

// The fake engine (FK-04). Every file under internal/jobs/testdata/r10k-5.0.3/ and
// internal/jobs/testdata/g10k-0.10.0/ is a verbatim copy of the file of the same
// name under stagehand-sdk-public harness/fixtures/ (real r10k 5.0.3 and g10k
// 0.10.0 runs recorded by the SDK harness). Do not edit them; re-copy them
// from the SDK.

import (
	"os"
	"testing"

	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/jobs/fakeengine"
)

func TestMain(m *testing.M) {
	fakeengine.MaybeRun() // becomes the fake r10k when the fixture env var is set
	maybeHoldLock()       // becomes a second process holding the deploy lock (lock_test.go)
	os.Exit(m.Run())
}
