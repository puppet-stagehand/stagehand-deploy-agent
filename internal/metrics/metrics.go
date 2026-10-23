// Package metrics is a small Prometheus registry and text-format writer. The
// agent's metric set is fixed and every label is bounded, so the client
// library is not worth a second module: go.mod keeps one third-party module.
//
// Three kinds are enough: counters, gauges (set directly or computed when
// scraped) and histograms. A family declares its label names when it is
// registered, so a label cannot be added at a call site. The writer emits the
// text format 0.0.4: one HELP and TYPE line per family, label values escaped,
// histogram buckets cumulative and ending in le="+Inf", and a fixed order so
// two scrapes of unchanged state are identical.
package metrics

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ContentType is the exposition's media type.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

var (
	metricNameRE = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	labelNameRE  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
)

type kind string

const (
	kindCounter   kind = "counter"
	kindGauge     kind = "gauge"
	kindHistogram kind = "histogram"
)

// sample is one series of a scrape-time gauge.
type sample struct {
	values []string
	value  float64
}

type series struct {
	values []string
	value  float64  // counter, gauge
	counts []uint64 // histogram: per bucket, not cumulative
	sum    float64
	count  uint64
}

type family struct {
	name, help string
	kind       kind
	labels     []string
	buckets    []float64 // histogram upper bounds, ascending, without +Inf

	mu     sync.Mutex
	series map[string]*series
	fn     func() (float64, bool) // scrape-time gauge with no labels
}

// Registry holds the families and the PD-33 recorders.
type Registry struct {
	mu       sync.Mutex
	families map[string]*family

	// The agent's own families; nil on a bare NewRegistry.
	httpRequests *Counter
	httpDuration *Histogram
	jobsTotal    *Counter
	jobDuration  *Histogram
	inFlight     *Gauge
	queued       *Gauge
	refused      *Counter
	tlsRefusals  *Counter

	clock func() time.Time

	srcMu   sync.Mutex
	readyFn func() bool
	certEnd func() (time.Time, bool)
	crlEnd  func() (time.Time, bool)
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{families: map[string]*family{}} }

func (r *Registry) register(name, help string, k kind, labels []string, buckets []float64) *family {
	if !metricNameRE.MatchString(name) {
		panic(fmt.Sprintf("metrics: bad metric name %q", name))
	}
	for _, l := range labels {
		if !labelNameRE.MatchString(l) || strings.HasPrefix(l, "__") || (k == kindHistogram && l == "le") || l == "le" {
			panic(fmt.Sprintf("metrics: bad label name %q on %s", l, name))
		}
	}
	f := &family{name: name, help: help, kind: k, labels: append([]string(nil), labels...), buckets: buckets, series: map[string]*series{}}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.families[name]; dup {
		panic(fmt.Sprintf("metrics: %s registered twice", name))
	}
	r.families[name] = f
	return f
}

// get returns the series for values, creating it. It panics when the number of
// values is not the number of declared labels: a programming error, caught by
// the first test that touches the call site.
func (f *family) get(values []string) *series {
	if len(values) != len(f.labels) {
		panic(fmt.Sprintf("metrics: %s takes %d label values, got %d", f.name, len(f.labels), len(values)))
	}
	key := strings.Join(values, "\xff")
	s, ok := f.series[key]
	if !ok {
		s = &series{values: append([]string(nil), values...)}
		if f.kind == kindHistogram {
			s.counts = make([]uint64, len(f.buckets))
		}
		f.series[key] = s
	}
	return s
}

// Counter is a monotonically increasing value per label set.
type Counter struct{ f *family }

// Counter registers a counter family.
func (r *Registry) Counter(name, help string, labels ...string) *Counter {
	return &Counter{r.register(name, help, kindCounter, labels, nil)}
}

// Inc adds one to the series for values.
func (c *Counter) Inc(values ...string) {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	c.f.get(values).value++
}

// Gauge is a value that goes up and down.
type Gauge struct{ f *family }

// Gauge registers a gauge family.
func (r *Registry) Gauge(name, help string, labels ...string) *Gauge {
	return &Gauge{r.register(name, help, kindGauge, labels, nil)}
}

// Set sets the series for values.
func (g *Gauge) Set(v float64, values ...string) {
	g.f.mu.Lock()
	defer g.f.mu.Unlock()
	g.f.get(values).value = v
}

// GaugeFunc registers an unlabelled gauge computed when it is scraped. When fn
// reports false the gauge has no sample in that scrape.
func (r *Registry) GaugeFunc(name, help string, fn func() (float64, bool)) {
	f := r.register(name, help, kindGauge, nil, nil)
	f.fn = fn
}

// Histogram counts observations into fixed buckets.
type Histogram struct{ f *family }

// Histogram registers a histogram family with the given ascending upper bounds.
func (r *Registry) Histogram(name, help string, buckets []float64, labels ...string) *Histogram {
	if !sort.Float64sAreSorted(buckets) {
		panic("metrics: histogram buckets must ascend: " + name)
	}
	return &Histogram{r.register(name, help, kindHistogram, labels, append([]float64(nil), buckets...))}
}

// Observe records v in the series for values.
func (h *Histogram) Observe(v float64, values ...string) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	s := h.f.get(values)
	for i, ub := range h.f.buckets {
		if v <= ub {
			s.counts[i]++
			break
		}
	}
	s.sum += v
	s.count++
}

// Labels returns every family's declared label names, for the test that pins
// them (T-57.6.2-36).
func (r *Registry) Labels() map[string][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string][]string, len(r.families))
	for name, f := range r.families {
		out[name] = append([]string(nil), f.labels...)
	}
	return out
}

// Handler serves the exposition.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", ContentType)
		_ = r.WriteText(w)
	})
}

// WriteText writes every family that has a sample, in name order, series in
// label-value order.
func (r *Registry) WriteText(w io.Writer) error {
	r.mu.Lock()
	fams := make([]*family, 0, len(r.families))
	for _, f := range r.families {
		fams = append(fams, f)
	}
	r.mu.Unlock()
	sort.Slice(fams, func(i, j int) bool { return fams[i].name < fams[j].name })
	bw := bufio.NewWriter(w)
	for _, f := range fams {
		f.write(bw)
	}
	return bw.Flush()
}

func (f *family) write(w *bufio.Writer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var rows []*series
	if f.fn != nil {
		v, ok := f.fn()
		if !ok {
			return
		}
		rows = []*series{{value: v}}
	} else {
		for _, s := range f.series {
			rows = append(rows, s)
		}
		sort.Slice(rows, func(i, j int) bool {
			return strings.Join(rows[i].values, "\xff") < strings.Join(rows[j].values, "\xff")
		})
	}
	if len(rows) == 0 {
		return
	}
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", f.name, escapeHelp(f.help), f.name, f.kind)
	for _, s := range rows {
		if f.kind != kindHistogram {
			fmt.Fprintf(w, "%s%s %s\n", f.name, labelSet(f.labels, s.values, ""), formatFloat(s.value))
			continue
		}
		var cum uint64
		for i, ub := range f.buckets {
			cum += s.counts[i]
			fmt.Fprintf(w, "%s_bucket%s %d\n", f.name, labelSet(f.labels, s.values, formatFloat(ub)), cum)
		}
		fmt.Fprintf(w, "%s_bucket%s %d\n", f.name, labelSet(f.labels, s.values, "+Inf"), s.count)
		fmt.Fprintf(w, "%s_sum%s %s\n", f.name, labelSet(f.labels, s.values, ""), formatFloat(s.sum))
		fmt.Fprintf(w, "%s_count%s %d\n", f.name, labelSet(f.labels, s.values, ""), s.count)
	}
}

// labelSet renders {a="x",b="y"} and, for a histogram bucket, a trailing le.
func labelSet(names, values []string, le string) string {
	if len(names) == 0 && le == "" {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, n := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(n + `="` + escapeLabel(values[i]) + `"`)
	}
	if le != "" {
		if len(names) > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`le="` + le + `"`)
	}
	b.WriteByte('}')
	return b.String()
}

var (
	labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	helpEscaper  = strings.NewReplacer(`\`, `\\`, "\n", `\n`)
)

func escapeLabel(s string) string { return labelEscaper.Replace(s) }
func escapeHelp(s string) string  { return helpEscaper.Replace(s) }

func formatFloat(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// The agent's own families (PD-33). The names are published: dashboards and
// alerts depend on them, and they are frozen at v1.0.0.
// Names are written out in full so a grep for one finds its definition.

var (
	httpBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}
	jobBuckets  = []float64{1, 5, 10, 30, 60, 120, 300, 600, 1800, 3600}

	jobKinds     = []string{"deploy", "remove"}
	jobOutcomes  = []string{"deployed", "failed", "refused"}
	refuseReason = []string{"busy", "shutdown", "protected", "invalid"}
	tlsReasons   = []string{"certname", "revoked", "fingerprint"}
)

// bounded folds a value outside the fixed set to "other", so a bug at a call
// site can never mint a new series.
func bounded(v string, allowed []string) string {
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	return "other"
}

// New returns a registry with the agent's families registered. version and
// engine are the build_info labels. clock is the time the expiry gauges are
// measured against; nil means time.Now.
func New(version, engine string, clock func() time.Time) *Registry {
	if clock == nil {
		clock = time.Now
	}
	r := NewRegistry()
	r.clock = clock

	r.Gauge("stagehand_deploy_agent_build_info", "Build information; the value is always 1.", "version", "engine").Set(1, version, engine)
	r.GaugeFunc("stagehand_deploy_agent_ready", "1 when /readyz would answer 200, otherwise 0.", func() (float64, bool) {
		r.srcMu.Lock()
		fn := r.readyFn
		r.srcMu.Unlock()
		if fn != nil && fn() {
			return 1, true
		}
		return 0, true
	})
	r.httpRequests = r.Counter("stagehand_deploy_agent_http_requests_total", "Requests answered on the mTLS port, by matched route and status code.", "route", "code")
	r.httpDuration = r.Histogram("stagehand_deploy_agent_http_request_duration_seconds", "Time to answer a request on the mTLS port, by matched route.", httpBuckets, "route")
	r.jobsTotal = r.Counter("stagehand_deploy_agent_jobs_total", "Jobs finished or refused, by kind and outcome.", "kind", "outcome")
	r.jobDuration = r.Histogram("stagehand_deploy_agent_job_duration_seconds", "Time a job held the deploy lock, by kind.", jobBuckets, "kind")
	r.inFlight = r.Gauge("stagehand_deploy_agent_jobs_in_flight", "Jobs holding the deploy lock now.")
	r.queued = r.Gauge("stagehand_deploy_agent_jobs_queued", "Jobs admitted and waiting for the deploy lock.")
	r.refused = r.Counter("stagehand_deploy_agent_requests_refused_total", "Requests refused for a reason other than a bad certificate.", "reason")
	r.tlsRefusals = r.Counter("stagehand_deploy_agent_tls_refusals_total", "Client certificates refused in the TLS handshake, by reason.", "reason")
	r.GaugeFunc("stagehand_deploy_agent_server_certificate_days_until_expiry", "Days until the server certificate expires; negative once it has.", r.daysFunc(func() func() (time.Time, bool) { return r.certEnd }))
	r.GaugeFunc("stagehand_deploy_agent_crl_days_until_expiry", "Days until the CRL's next update; negative once it has passed. Absent without a CRL.", r.daysFunc(func() func() (time.Time, bool) { return r.crlEnd }))
	r.inFlight.Set(0)
	r.queued.Set(0)
	return r
}

// daysFunc builds a scrape-time gauge from whichever source pick returns now.
func (r *Registry) daysFunc(pick func() func() (time.Time, bool)) func() (float64, bool) {
	return func() (float64, bool) {
		r.srcMu.Lock()
		src := pick()
		r.srcMu.Unlock()
		if src == nil {
			return 0, false
		}
		end, ok := src()
		if !ok {
			return 0, false
		}
		return end.Sub(r.clock()).Hours() / 24, true
	}
}

// SetReadySource sets what the ready gauge reports; until it is set the gauge
// reads 0.
func (r *Registry) SetReadySource(fn func() bool) {
	r.srcMu.Lock()
	r.readyFn = fn
	r.srcMu.Unlock()
}

// SetExpirySources sets where the expiry gauges read the server certificate's
// NotAfter and the CRL's NextUpdate from, at scrape time. A nil source, or one
// that reports false, leaves its gauge out of the scrape.
func (r *Registry) SetExpirySources(cert, crl func() (time.Time, bool)) {
	r.srcMu.Lock()
	r.certEnd, r.crlEnd = cert, crl
	r.srcMu.Unlock()
}

// ObserveRequest records one answered request. route is the matched ServeMux
// pattern, or "unmatched".
func (r *Registry) ObserveRequest(route string, code int, d time.Duration) {
	r.httpRequests.Inc(route, strconv.Itoa(code))
	r.httpDuration.Observe(d.Seconds(), route)
}

// TLSRefusal records a client certificate refused in the handshake.
func (r *Registry) TLSRefusal(reason string) {
	r.tlsRefusals.Inc(bounded(reason, tlsReasons))
}

// JobStarted is told when a job takes the deploy lock. Nothing is recorded
// here: in-flight and queued are set together by Queue, so they never disagree.
func (r *Registry) JobStarted(kind string) {}

// JobFinished records a job's outcome (deployed or failed). The duration is
// observed only when it is positive: a job that never held the lock (a
// shutdown failed it while it waited) is counted but has no duration.
func (r *Registry) JobFinished(kind, outcome string, d time.Duration) {
	kind = bounded(kind, jobKinds)
	r.jobsTotal.Inc(kind, bounded(outcome, jobOutcomes))
	if d > 0 {
		r.jobDuration.Observe(d.Seconds(), kind)
	}
}

// Refused records a refused request. A refusal because the runner would not
// take a job (busy, shutdown) also counts as a refused job; a protected or
// invalid request never became one.
func (r *Registry) Refused(kind, reason string) {
	reason = bounded(reason, refuseReason)
	r.refused.Inc(reason)
	if reason == "busy" || reason == "shutdown" {
		r.jobsTotal.Inc(bounded(kind, jobKinds), "refused")
	}
}

// Queue sets the jobs waiting for the deploy lock and the jobs holding it.
func (r *Registry) Queue(queued, inFlight int) {
	r.queued.Set(float64(queued))
	r.inFlight.Set(float64(inFlight))
}
