package metrics

import "github.com/prometheus/client_golang/prometheus"

// RecentsActivityFailures counts the personal recents activities lost to a
// failure: a row write that failed, or an OnlyOffice session store failure that
// loses an opening. A lost activity is accepted for a recents list (ADR), so it
// is only watched here. An activity lost by design (a member on another stack)
// is not counted.
var RecentsActivityFailures = prometheus.NewCounter(
	prometheus.CounterOpts{
		Namespace: "recents",
		Subsystem: "activity",
		Name:      "failures_total",

		Help: "Number of personal recents activities lost to a failure",
	},
)

func init() {
	prometheus.MustRegister(RecentsActivityFailures)
}
