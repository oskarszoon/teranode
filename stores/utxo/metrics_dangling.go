package utxo

import (
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// prometheusDanglingSpenderRef counts conflict-resolution reads that followed a
// spender reference (or conflicting-child entry) to an absent record — the
// dangling-spender-reference condition. Detection only; the readers still
// return the underlying ErrTxNotFound.
var prometheusDanglingSpenderRef = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "teranode",
	Subsystem: "utxo",
	Name:      "dangling_spender_ref_total",
	Help:      "Conflict-resolution reads that followed a reference to an absent tx record, by site",
}, []string{"site"})

// recordDanglingSpenderRef bumps the counter iff err is a tx-not-found — i.e. a
// followed reference resolved to a missing record.
func recordDanglingSpenderRef(site string, err error) {
	if errors.Is(err, errors.ErrTxNotFound) {
		prometheusDanglingSpenderRef.WithLabelValues(site).Inc()
	}
}
