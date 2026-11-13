package metrics

import "github.com/prometheus/client_golang/prometheus"

// RecentsSeedFailures counts the failed seedings of the personal recents.
var RecentsSeedFailures = prometheus.NewCounter(
	prometheus.CounterOpts{
		Namespace: "recents",
		Subsystem: "seed",
		Name:      "failures_total",

		Help: "Number of failed personal recents seedings",
	},
)

func init() {
	prometheus.MustRegister(RecentsSeedFailures)
}
