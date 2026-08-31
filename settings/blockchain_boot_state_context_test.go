package settings

import (
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// requireNoBootStateEnv makes the settings.conf-default tests hermetic.
//
// gocore checks the environment before the config file (config.go:621), and this
// key is explicitly meant to be supplied via env — the kubernetes operator
// configmap does exactly that. So an ambient blockchain_initializeNodeInState in
// a developer's or CI shell would silently shadow settings.conf and fail these
// tests for the wrong reason. Unset it for the duration, restoring any prior
// value afterwards.
func requireNoBootStateEnv(t *testing.T) {
	t.Helper()

	const key = "blockchain_initializeNodeInState"

	if orig, ok := os.LookupEnv(key); ok {
		require.NoError(t, os.Unsetenv(key))
		t.Cleanup(func() { require.NoError(t, os.Setenv(key, orig)) })
	}
}

// TestInitializeNodeInState_ContextDefaults pins the settings.conf defaults for
// blockchain_initializeNodeInState.
//
// Production contexts boot IDLE so an operator can verify a seed before the node
// consumes network or mutates state; dev and test contexts keep the empty
// default so local iteration and CI start catching up with no manual step.
//
// The dev/test half of this table is the important half. gocore resolves a
// context key by stripping dot-separated segments from the right, so
// blockchain_initializeNodeInState.docker.m falls back through
// blockchain_initializeNodeInState.docker before reaching the bare key. Setting
// the default on .docker instead of .docker.m would therefore be inherited by
// every dev and e2e compose stack, silently stopping them from syncing.
func TestInitializeNodeInState_ContextDefaults(t *testing.T) {
	requireNoBootStateEnv(t)

	const key = "blockchain_initializeNodeInState"

	tests := []struct {
		context string
		want    string
		why     string
	}{
		{context: "operator", want: "IDLE", why: "kubernetes operator deployments boot quiescent"},
		{context: "operator.mainnet", want: "IDLE", why: "operator sub-contexts inherit the IDLE default"},
		{context: "operator.testnet", want: "IDLE", why: "operator sub-contexts inherit the IDLE default"},
		{context: "operator.teratestnet", want: "IDLE", why: "operator sub-contexts inherit the IDLE default"},
		{context: "operator.teratestnet.prod", want: "IDLE", why: "operator sub-contexts inherit the IDLE default"},
		{context: "docker.m", want: "IDLE", why: "single-node quickstart stack boots quiescent"},

		{context: "dev", want: "", why: "dev must keep booting into CATCHINGBLOCKS"},
		{context: "test", want: "", why: "long tests must keep booting into CATCHINGBLOCKS"},
		{context: "docker", want: "", why: "dev compose stacks must not inherit the docker.m default"},
		{context: "docker.ci", want: "", why: "CI must keep booting into CATCHINGBLOCKS"},
		{context: "docker.teranode1.test", want: "", why: "e2e compose stacks must keep booting into CATCHINGBLOCKS"},
		{context: "docker.host.teranode1", want: "", why: "host compose stacks must keep booting into CATCHINGBLOCKS"},
	}

	for _, tt := range tests {
		t.Run(tt.context, func(t *testing.T) {
			require.Equal(t, tt.want, getString(key, "", tt.context), tt.why)
		})
	}
}

// TestInitializeNodeInState_ReachesSettingsStruct closes the gap between the
// settings.conf defaults above and what the blockchain service actually reads.
// The table above exercises raw key resolution; this pins that NewSettings
// carries the resolved value through to the struct field Init consults, so a
// future refactor of settings.go cannot silently disconnect the default from the
// code that uses it.
func TestInitializeNodeInState_ReachesSettingsStruct(t *testing.T) {
	requireNoBootStateEnv(t)

	tests := []struct {
		context string
		want    string
	}{
		{context: "operator", want: "IDLE"},
		{context: "docker.m", want: "IDLE"},
		{context: "dev", want: ""},
		{context: "test", want: ""},
		{context: "docker", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.context, func(t *testing.T) {
			require.Equal(t, tt.want, NewSettings(tt.context).BlockChain.InitializeNodeInState)
		})
	}
}

// TestInitializeNodeInState_TrimsWhitespace pins the TrimSpace at the read site.
// gocore trims values it parses out of settings.conf but returns environment
// variables verbatim, and the kubernetes operator configmap supplies this key as
// an env var. Since an unrecognised value deliberately fails startup, an
// unnoticed trailing space would otherwise brick a boot on a valid value — the
// same reason grpc_admin_api_key is trimmed at its read site.
func TestInitializeNodeInState_TrimsWhitespace(t *testing.T) {
	for _, raw := range []string{" IDLE", "IDLE ", "  IDLE  ", "\tIDLE\n"} {
		t.Run(fmt.Sprintf("%q", raw), func(t *testing.T) {
			t.Setenv("blockchain_initializeNodeInState", raw)
			require.Equal(t, "IDLE", NewSettings("operator").BlockChain.InitializeNodeInState)
		})
	}
}
