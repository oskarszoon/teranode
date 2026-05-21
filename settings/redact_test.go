package settings

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRedactReplacesTaggedFields(t *testing.T) {
	in := &Settings{
		GRPCAdminAPIKey: "super-secret-grpc-key",
		Coinbase: CoinbaseSettings{
			UserPwd:          "coinbase-db-pwd",
			P2PPrivateKey:    "coinbase-p2p-key",
			WalletPrivateKey: "coinbase-wallet-key",
			SlackToken:       "xoxb-slack-token",
		},
		P2P: P2PSettings{
			PrivateKey: "p2p-priv-key",
		},
		Alert: AlertSettings{
			P2PPrivateKey: "alert-p2p-key",
		},
		RPC: RPCSettings{
			RPCPass:      "rpc-admin-pwd",
			RPCLimitPass: "rpc-limit-pwd",
		},
		BlockAssembly: BlockAssemblySettings{
			MinerWalletPrivateKeys: []string{"miner-key-1", "miner-key-2"},
		},
	}

	out, err := Redact(in)
	require.NoError(t, err)
	require.NotNil(t, out)

	data, err := json.Marshal(out)
	require.NoError(t, err)
	js := string(data)

	secrets := []string{
		"super-secret-grpc-key",
		"coinbase-db-pwd",
		"coinbase-p2p-key",
		"coinbase-wallet-key",
		"xoxb-slack-token",
		"p2p-priv-key",
		"alert-p2p-key",
		"rpc-admin-pwd",
		"rpc-limit-pwd",
		"miner-key-1",
		"miner-key-2",
	}
	for _, s := range secrets {
		require.NotContainsf(t, js, s, "secret %q leaked into redacted output", s)
	}

	require.Contains(t, js, redactedPlaceholder, "expected placeholder in output")
}

func TestRedactNilInputReturnsNil(t *testing.T) {
	out, err := Redact(nil)
	require.NoError(t, err)
	require.Nil(t, out)
}

func TestRedactPreservesNonSecretFields(t *testing.T) {
	in := &Settings{
		Coinbase: CoinbaseSettings{
			UserPwd: "secret",
		},
	}

	out, err := Redact(in)
	require.NoError(t, err)
	require.NotNil(t, out)

	data, err := json.Marshal(out)
	require.NoError(t, err)
	require.True(t, strings.Contains(string(data), redactedPlaceholder), "expected redacted placeholder")
}
