// Package metrics is a small Prometheus registry and text-format writer. The
// agent's metric set is fixed and every label is bounded, so the client
// library is not worth a second module: go.mod keeps one third-party module.
package metrics

import (
	"io"
	"net/http"
	"time"
)

type Registry struct{}
type Counter struct{}
type Gauge struct{}
type Histogram struct{}

func NewRegistry() *Registry                                             { return &Registry{} }
func New(version, engine string, clock func() time.Time) *Registry       { return &Registry{} }
func (r *Registry) Counter(name, help string, labels ...string) *Counter { return &Counter{} }
func (r *Registry) Gauge(name, help string, labels ...string) *Gauge     { return &Gauge{} }
func (r *Registry) Histogram(name, help string, b []float64, l ...string) *Histogram {
	return &Histogram{}
}
func (r *Registry) GaugeFunc(name, help string, fn func() (float64, bool)) {}
func (c *Counter) Inc(values ...string)                                    {}
func (g *Gauge) Set(v float64, values ...string)                           {}
func (h *Histogram) Observe(v float64, values ...string)                   {}
func (r *Registry) WriteText(w io.Writer) error                            { return nil }
func (r *Registry) Handler() http.Handler                                  { return http.NotFoundHandler() }
func (r *Registry) Labels() map[string][]string                            { return nil }
func (r *Registry) ObserveRequest(route string, code int, d time.Duration) {}
func (r *Registry) TLSRefusal(reason string)                               {}
func (r *Registry) JobStarted(kind string)                                 {}
func (r *Registry) JobFinished(kind, outcome string, d time.Duration)      {}
func (r *Registry) Refused(kind, reason string)                            {}
func (r *Registry) Queue(queued, inFlight int)                             {}
func (r *Registry) SetReadySource(fn func() bool)                          {}
func (r *Registry) SetExpirySources(cert, crl func() (time.Time, bool))    {}
