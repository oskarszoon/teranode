package netsync

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// TestOrphanParked_CountsByReason verifies that recordOrphanParked increments
// prometheusLegacyOrphanParked under the correct reason label, restoring the
// visibility that was lost when orphan-park events were only logged at Debug
// with no metric (issue #1214: parked-and-evaporated orphans were invisible
// for ~40h during the eu-1 incident).
func TestOrphanParked_CountsByReason(t *testing.T) {
	initPrometheusMetrics()

	before := testutil.ToFloat64(prometheusLegacyOrphanParked.WithLabelValues("missing-parent"))
	recordOrphanParked("missing-parent")
	after := testutil.ToFloat64(prometheusLegacyOrphanParked.WithLabelValues("missing-parent"))
	require.Equal(t, before+1, after)
}

// TestOrphanParked_CountsByReason_Locked verifies the "locked" reason label is
// tracked independently from "missing-parent".
func TestOrphanParked_CountsByReason_Locked(t *testing.T) {
	initPrometheusMetrics()

	before := testutil.ToFloat64(prometheusLegacyOrphanParked.WithLabelValues("locked"))
	recordOrphanParked("locked")
	after := testutil.ToFloat64(prometheusLegacyOrphanParked.WithLabelValues("locked"))
	require.Equal(t, before+1, after)
}
