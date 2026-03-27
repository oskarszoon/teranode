// Package blockvalidation provides the HTTPTransport implementation of CatchupTransport.
package blockvalidation

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/services/blockvalidation/catchup"
	"github.com/bsv-blockchain/teranode/util"
)

// HTTPTransport implements CatchupTransport using the peer's DataHub HTTP endpoints.
// It is stateless and safe for concurrent use.
type HTTPTransport struct{}

// NewHTTPTransport returns a new HTTPTransport.
func NewHTTPTransport() *HTTPTransport {
	return &HTTPTransport{}
}

// FetchHeaders implements CatchupTransport.
// It calls GET {baseURL}/headers_from_common_ancestor/{targetHash}?block_locator_hashes=...&n=maxHeaders
// and parses the response bytes into model.BlockHeader values.
func (t *HTTPTransport) FetchHeaders(ctx context.Context, baseURL string, targetHash *chainhash.Hash, locatorHashes []*chainhash.Hash, maxHeaders int) ([]*model.BlockHeader, error) {
	locatorStr := catchup.BuildBlockLocatorString(locatorHashes)
	url := fmt.Sprintf("%s/headers_from_common_ancestor/%s?block_locator_hashes=%s&n=%d",
		baseURL, targetHash.String(), locatorStr, maxHeaders)

	headerBytes, err := util.DoHTTPRequest(ctx, url)
	if err != nil {
		return nil, errors.NewProcessingError("[HTTPTransport.FetchHeaders] failed to fetch headers from %s", baseURL, err)
	}

	if err = catchup.ValidateBlockHeaderBytes(headerBytes); err != nil {
		return nil, err
	}

	return catchup.ParseBlockHeaders(headerBytes)
}

// FetchBlock implements CatchupTransport.
// It calls GET {baseURL}/block/{hash} and parses the response as a model.Block.
func (t *HTTPTransport) FetchBlock(ctx context.Context, baseURL string, hash *chainhash.Hash) (*model.Block, error) {
	blockBytes, err := util.DoHTTPRequest(ctx, fmt.Sprintf("%s/block/%s", baseURL, hash.String()))
	if err != nil {
		return nil, errors.NewProcessingError("[HTTPTransport.FetchBlock] failed to fetch block %s from %s", hash.String(), baseURL, err)
	}

	block, err := model.NewBlockFromReader(bytes.NewReader(blockBytes))
	if err != nil {
		return nil, errors.NewProcessingError("[HTTPTransport.FetchBlock] failed to parse block %s", hash.String(), err)
	}

	return block, nil
}

// FetchBlocks implements CatchupTransport.
// It calls GET {baseURL}/blocks/{hash}?n={n} and parses all blocks from the response.
func (t *HTTPTransport) FetchBlocks(ctx context.Context, baseURL string, hash *chainhash.Hash, n uint32) ([]*model.Block, error) {
	blockBytes, err := util.DoHTTPRequest(ctx, fmt.Sprintf("%s/blocks/%s?n=%d", baseURL, hash.String(), n))
	if err != nil {
		return nil, errors.NewProcessingError("[HTTPTransport.FetchBlocks] failed to fetch blocks from %s", baseURL, err)
	}

	reader := bytes.NewReader(blockBytes)
	blocks := make([]*model.Block, 0)

	for {
		block, err := model.NewBlockFromReader(reader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, errors.NewProcessingError("[HTTPTransport.FetchBlocks] failed to parse block at offset", err)
		}
		blocks = append(blocks, block)
	}

	return blocks, nil
}

// FetchSubtree implements CatchupTransport.
// It calls GET {baseURL}/subtree/{hash} and returns the raw bytes.
func (t *HTTPTransport) FetchSubtree(ctx context.Context, baseURL string, hash *chainhash.Hash) ([]byte, error) {
	subtreeBytes, err := util.DoHTTPRequest(ctx, fmt.Sprintf("%s/subtree/%s", baseURL, hash.String()))
	if err != nil {
		return nil, errors.NewProcessingError("[HTTPTransport.FetchSubtree] failed to fetch subtree %s from %s", hash.String(), baseURL, err)
	}
	return subtreeBytes, nil
}

// FetchSubtreeData implements CatchupTransport.
// It calls GET {baseURL}/subtree_data/{hash} and returns the response body as a stream.
// The caller must close the returned ReadCloser.
func (t *HTTPTransport) FetchSubtreeData(ctx context.Context, baseURL string, hash *chainhash.Hash) (io.ReadCloser, error) {
	reader, err := util.DoHTTPRequestBodyReader(ctx, fmt.Sprintf("%s/subtree_data/%s", baseURL, hash.String()))
	if err != nil {
		return nil, errors.NewProcessingError("[HTTPTransport.FetchSubtreeData] failed to fetch subtree data %s from %s", hash.String(), baseURL, err)
	}
	return reader, nil
}
