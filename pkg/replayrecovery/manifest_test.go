package replayrecovery

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type unavailableInputs struct{ *recordBackend }

func (b unavailableInputs) Inputs(context.Context, string) ([]string, error) {
	return nil, failure("external bytes unavailable")
}

func TestIncompleteGraphRemainsExportableButCannotApply(t *testing.T) {
	b, s, a, _, path, journal := recoveryFixture(t)
	backend := unavailableInputs{b}
	_, err := Discover(t.Context(), backend, s, a, path, nil)
	require.ErrorIs(t, err, ErrIncomplete)
	var out bytes.Buffer
	require.NoError(t, ExportManifest(t.Context(), path, &out))
	require.Contains(t, out.String(), `"graph_complete":false`)
	_, err = Apply(t.Context(), backend, s, path, journal, ApplyOptions{Maintenance: true, Tip: s.tip})
	require.Error(t, err)
	require.Zero(t, b.writes)
}
