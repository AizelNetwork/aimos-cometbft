package core

import (
	ctypes "github.com/cometbft/cometbft/v2/rpc/core/types"
	rpctypes "github.com/cometbft/cometbft/v2/rpc/jsonrpc/types"
)

// UnsafeFlushMempool removes all transactions from the mempool.
func (env *Environment) UnsafeFlushMempool(*rpctypes.Context) (*ctypes.ResultUnsafeFlushMempool, error) {
	env.Mempool.Flush()
	return &ctypes.ResultUnsafeFlushMempool{}, nil
}
