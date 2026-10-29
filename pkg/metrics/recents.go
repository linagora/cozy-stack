package metrics

import "github.com/prometheus/client_golang/prometheus"

// RecentsWriteFailures counts the personal recents row writes that failed.
// A failure is a lost activity, accepted for a recents list (ADR), so it is
// only watched here.
var RecentsWriteFailures = prometheus.NewCounter(
	prometheus.CounterOpts{
		Namespace: "recents",
		Subsystem: "rows",
		Name:      "write_failures_total",

		Help: "Number of failed personal recents row writes (lost activities)",
	},
)

func init() {
	prometheus.MustRegister(RecentsWriteFailures)
}
