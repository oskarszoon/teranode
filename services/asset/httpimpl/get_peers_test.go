package httpimpl

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPeerInfoResponse_JSON covers the JSON shape the dashboard expects so any
// future field-rename refactor breaks visibly rather than silently.
func TestPeerInfoResponse_JSON(t *testing.T) {
	r := PeerInfoResponse{
		ID:                     "12D3KooWTest",
		ClientName:             "tn",
		Height:                 99,
		BlockHash:              "deadbeef",
		DataHubURL:             "http://example.com",
		BanScore:               5,
		IsBanned:               false,
		IsConnected:            true,
		ConnectedAt:            1700000000,
		BytesReceived:          2048,
		LastBlockTime:          1700000005,
		LastMessageTime:        1700000010,
		CatchupAttempts:        12,
		CatchupSuccesses:       11,
		CatchupFailures:        1,
		CatchupReputationScore: 80.5,
		CatchupMaliciousCount:  0,
		CatchupAvgResponseTime: 150,
		LastCatchupError:       "",
		LastCatchupErrorTime:   0,
	}
	data, err := json.Marshal(r)
	require.NoError(t, err)

	// Spot-check key fields are emitted with the expected JSON tags.
	require.Contains(t, string(data), `"id":"12D3KooWTest"`)
	require.Contains(t, string(data), `"client_name":"tn"`)
	require.Contains(t, string(data), `"data_hub_url":"http://example.com"`)
	require.Contains(t, string(data), `"is_connected":true`)
	require.Contains(t, string(data), `"catchup_avg_response_ms":150`)
}

// TestPeersResponse_JSON sanity-checks the wrapper struct's JSON shape.
func TestPeersResponse_JSON(t *testing.T) {
	r := PeersResponse{
		Peers: []PeerInfoResponse{{ID: "x"}},
		Count: 1,
	}
	data, err := json.Marshal(r)
	require.NoError(t, err)
	require.Contains(t, string(data), `"peers":[`)
	require.Contains(t, string(data), `"count":1`)
}
