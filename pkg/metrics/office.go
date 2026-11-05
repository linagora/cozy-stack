package metrics

import "github.com/prometheus/client_golang/prometheus"

// OfficeForceSaveUnconfirmed counts the downloads served without a confirmed
// forcesave, labelled by reason.
var OfficeForceSaveUnconfirmed = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "office",
		Subsystem: "forcesave",
		Name:      "unconfirmed",

		Help: `Number of downloads served without a confirmed forcesave, so maybe without the
last changes of an OnlyOffice editing session, labelled by reason: "timeout" and
"error" for a file with a session, "reread" when the saved file cannot be read,
"store" for any download when the session store fails.`,
	},
	[]string{"reason"},
)

func init() {
	prometheus.MustRegister(OfficeForceSaveUnconfirmed)
}
