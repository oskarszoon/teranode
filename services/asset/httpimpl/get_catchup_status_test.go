package httpimpl

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/services/blockvalidation"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestGetCatchupStatus_Success(t *testing.T) {
	httpServer, mockRepo, c, rec := GetMockHTTP(t, nil)

	mockBV := &blockvalidation.Mock{}
	mockRepo.On("GetBlockvalidationClient").Return(mockBV)

	status := &blockvalidation.CatchupStatus{
		IsCatchingUp:         true,
		PeerID:               "peer-A",
		PeerURL:              "http://peer-a",
		TargetBlockHash:      "deadbeef",
		TargetBlockHeight:    100,
		CurrentHeight:        50,
		TotalBlocks:          50,
		BlocksFetched:        25,
		BlocksValidated:      20,
		StartTime:            1700000000,
		DurationMs:           5000,
		ForkDepth:            2,
		CommonAncestorHash:   "deadbe00",
		CommonAncestorHeight: 49,
		Phase:                "validating_blocks",
	}
	mockBV.On("GetCatchupStatus", mock.Anything).Return(status, nil)

	err := httpServer.GetCatchupStatus(c)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, true, resp["is_catching_up"])
	require.Equal(t, "peer-A", resp["peer_id"])
	require.Equal(t, "validating_blocks", resp["phase"])
}

func TestGetCatchupStatus_WithPreviousAttempt(t *testing.T) {
	httpServer, mockRepo, c, rec := GetMockHTTP(t, nil)

	mockBV := &blockvalidation.Mock{}
	mockRepo.On("GetBlockvalidationClient").Return(mockBV)

	status := &blockvalidation.CatchupStatus{
		IsCatchingUp: false,
		PreviousAttempt: &blockvalidation.PreviousAttempt{
			PeerID:            "peer-prev",
			PeerURL:           "http://peer-prev",
			TargetBlockHash:   "deadbeef",
			TargetBlockHeight: 99,
			ErrorMessage:      "timeout",
			ErrorType:         "network_error",
			AttemptTime:       1699999000,
			DurationMs:        3000,
			BlocksValidated:   10,
		},
	}
	mockBV.On("GetCatchupStatus", mock.Anything).Return(status, nil)

	err := httpServer.GetCatchupStatus(c)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	prev, ok := resp["previous_attempt"].(map[string]interface{})
	require.True(t, ok, "previous_attempt key must be present")
	require.Equal(t, "peer-prev", prev["peer_id"])
	require.Equal(t, "timeout", prev["error_message"])
}

func TestGetCatchupStatus_ClientReturnsError(t *testing.T) {
	httpServer, mockRepo, c, rec := GetMockHTTP(t, nil)

	mockBV := &blockvalidation.Mock{}
	mockRepo.On("GetBlockvalidationClient").Return(mockBV)
	mockBV.On("GetCatchupStatus", mock.Anything).
		Return((*blockvalidation.CatchupStatus)(nil), errors.NewServiceError("boom"))

	err := httpServer.GetCatchupStatus(c)
	require.NoError(t, err)
	require.Equal(t, http.StatusInternalServerError, rec.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, false, resp["is_catching_up"])
	require.Contains(t, resp["error"], "Failed to get catchup status")
}

// keep imports referenced
var _ = context.Background
var _ = httptest.NewRecorder
var _ = echo.New
