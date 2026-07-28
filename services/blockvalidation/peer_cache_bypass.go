package blockvalidation

import (
	"fmt"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
)

// cacheBypassRetryableKey marks an error whose cause is a peer response that a
// caching layer in front of that peer may have poisoned — specifically a 200 whose
// body is empty or shorter than the subtree it belongs to requires.
//
// Peers front the asset service with an nginx proxy_cache that caches any 200 for
// the configured TTL. An aborted or failed on-demand subtree_data generation that
// still reached the client as "200 + empty body" is stored as a valid response and
// replayed to every requester, which is what stalled catchup in issue 1368. An
// error carrying this marker tells the fetch loop that one retry with a
// cache-busting URL is worth attempting before moving on to another peer.
const cacheBypassRetryableKey = "cache_bypass_retryable"

// markCacheBypassRetryable tags err with the cache-bypass marker in place and
// returns it. The error code and wrapped chain are unchanged, so callers matching
// on the code still behave identically. Nil-safe. A non-native error is returned
// untouched (nothing downstream can carry the marker for it).
func markCacheBypassRetryable(err error) error {
	if err == nil {
		return nil
	}

	var e *errors.Error
	if errors.As(err, &e) {
		e.SetData(cacheBypassRetryableKey, true)
	}

	return err
}

// isCacheBypassRetryable reports whether any link in err's chain carries the
// cache-bypass marker. The walk mirrors catchupFailureAlreadyReported: the marker
// must be found mid-chain because every layer above the fetch wraps the error.
func isCacheBypassRetryable(err error) bool {
	var e *errors.Error
	if !errors.As(err, &e) {
		return false
	}

	for depth := 0; e != nil && depth < 32; depth++ {
		if v, ok := e.GetData(cacheBypassRetryableKey).(bool); ok && v {
			return true
		}

		next := e.WrappedErr()
		if next == nil {
			return false
		}

		var wrapped *errors.Error
		if !errors.As(next, &wrapped) {
			return false
		}

		e = wrapped
	}

	return false
}

// peerResourceURL builds the URL for a peer asset endpoint.
//
// With bypassCache set, a unique cachebust query parameter is appended. A peer's
// nginx cache key includes $request_uri (deploy/docker/base/asset-cache-nginx.conf),
// while nginx location matching ignores the query string — so the busted URL reaches
// the same handler but misses the cache, forcing a fresh on-demand generation. This
// needs no change on the peer, which is the only lever available against a fleet we
// cannot update.
func (u *Server) peerResourceURL(baseURL, resource string, hash *chainhash.Hash, bypassCache bool) string {
	url := fmt.Sprintf("%s/%s/%s", baseURL, resource, hash.String())
	if !bypassCache {
		return url
	}

	return fmt.Sprintf("%s?cachebust=%d", url, u.cacheBustCounter.Add(1))
}

// newPoisonedSubtreeDataError builds the error returned when a peer answers a
// subtree_data request with 200 but a body that cannot satisfy the subtree.
//
// Classified ErrExternal: no local component failed, so errors.IsLocalError is false
// and the caller still tries alternative peers. Carries the cache-bypass marker so
// the same peer is retried once with a cache-busting URL.
func newPoisonedSubtreeDataError(peerID, baseURL string, subtreeHash *chainhash.Hash, missing, expected int, bytesRead uint64) error {
	var e *errors.Error

	if bytesRead == 0 {
		e = errors.NewExternalError("[catchup:fetchAndStoreSubtreeData] peer %s (%s) served empty subtree_data for %s (HTTP 200, 0 bytes, expected %d txs) - poisoned cache entry or aborted on-demand generation", peerID, baseURL, subtreeHash.String(), expected)
	} else {
		e = errors.NewExternalError("[catchup:fetchAndStoreSubtreeData] peer %s (%s) served incomplete subtree_data for %s (%d bytes, %d of %d txs missing)", peerID, baseURL, subtreeHash.String(), bytesRead, missing, expected)
	}

	e.SetData(cacheBypassRetryableKey, true)

	return e
}
