// Package contract holds the proof that the agent's /api/v1/deploys answers
// are the webhook-go fork's answers, byte for byte, apart from the differences
// listed here.
//
// The fork's responses were recorded from its own gin handlers at
// v2.15.1-stagehand.1 (hack/record-golden.sh) into contract/golden. The replay
// test (golden_test.go) holds the agent to those bytes for every case NOT in
// Divergences, and holds it to the Divergences entry for every case that is. A
// new difference therefore cannot slip in silently: it fails the replay until
// someone writes it down here, with a reason.
//
// The Code Manager pack 0.1.0 evidence for each entry (PD-10) is the pack's
// internal/deploy/webhook/client.go at 855c652: Deploy builds its request body
// with json.Marshal (lines 403-406), so there is never data after the object;
// a request the service accepts names at most 20 environments, a few KiB; and
// every status the client does not know becomes "the deploy service answered
// HTTP N" (httpReason, line 260; Deploy line 459, Probe line 257, look line
// 514). The only paths it requests are POST /api/v1/deploys (deploysPath, line
// 57), GET /api/v1/deploys?limit=N (Probe, discover) and
// GET /api/v1/deploys/{id} (look).
package contract

// Divergence is one intentional difference from the fork.
type Divergence struct {
	Status      int    // the agent's status
	ContentType string // the agent's Content-Type
	Body        string // the agent's exact body
	Reason      string // why it differs, and why the pack is unaffected
}

// Divergences maps a golden case name to how the agent intentionally answers
// instead of the fork. Exactly these four cases differ.
var Divergences = map[string]Divergence{
	"post_oversized": {
		Status:      413,
		ContentType: "application/json; charset=utf-8",
		Body:        `{"error":"request body exceeds 65536 bytes"}`,
		Reason: "FORK-IN-06: the fork answered 400 'not valid JSON: http: request body too large' for a body over 64 KiB, " +
			"which blames the caller's syntax for a size problem. PD-10: Deploy (client.go:403-411) marshals one entry per " +
			"environment, a few KiB for the at most 20 the service accepts, so it never reaches the cap, and a 413 would " +
			"become 'the deploy service answered HTTP 413' (client.go:459).",
	},
	"post_trailing_data": {
		Status:      400,
		ContentType: "application/json; charset=utf-8",
		Body:        `{"error":"request body is not valid JSON: unexpected data after the JSON object"}`,
		Reason: "FORK-IN-06: the fork decoded the first JSON value and ignored anything after it (202), so a request with " +
			"junk behind the object was accepted. PD-10: the pack sends exactly the json.Marshal output (client.go:403-411), " +
			"with nothing after the object; unknown fields inside the object are still tolerated.",
	},
	"unknown_path": {
		Status:      404,
		ContentType: "text/plain; charset=utf-8",
		Body:        "404 page not found\n",
		Reason: "net/http's ServeMux answers unknown paths and wrong methods itself (404 with a trailing newline and a " +
			"charset; 405 for a known path with another method) where gin answered 404 'text/plain' without the newline. " +
			"PD-10: the pack's client only requests the three routes the agent serves (client.go:57, 243, 469, 503), so it " +
			"never sees this answer.",
	},
	"deploys_trailing_slash": {
		Status:      404,
		ContentType: "text/plain; charset=utf-8",
		Body:        "404 page not found\n",
		Reason: "gin redirected /api/v1/deploys/ to /api/v1/deploys with a 301; the ServeMux has no such pattern and " +
			"answers 404. PD-10: the pack's client builds its URLs from deploysPath (client.go:57) and deploysPath+\"/\"+id " +
			"(client.go:503), never with a trailing slash, so it never meets the redirect.",
	},
}
