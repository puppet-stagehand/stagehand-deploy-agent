// Command fakeengine is the fake r10k/g10k as a standalone binary, for
// out-of-process runs (hack/smoke.sh). Point engine.command_path at it and set
// the STAGEHAND_FAKE_ENGINE_* variables; see package fakeengine. Test
// infrastructure only: the agent never ships it.
package main

import (
	"fmt"
	"os"

	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/jobs/fakeengine"
)

func main() {
	fakeengine.MaybeRun() // replays the fixture and exits when it is configured
	fmt.Fprintln(os.Stderr, "fakeengine: set STAGEHAND_FAKE_ENGINE_FIXTURE or STAGEHAND_FAKE_ENGINE_MODE")
	os.Exit(2)
}
