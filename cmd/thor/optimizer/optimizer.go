// Copyright (c) 2019 The VeChainThor developers

// Distributed under the GNU Lesser General Public License v3.0 software license, see the accompanying
// file LICENSE or <https://www.gnu.org/licenses/lgpl-3.0.html>

package optimizer

import (
	"context"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/rlp"
	"github.com/inconshreveable/log15"
	"github.com/vechain/thor/block"
	"github.com/vechain/thor/builtin"
	"github.com/vechain/thor/chain"
	"github.com/vechain/thor/co"
	"github.com/vechain/thor/muxdb"
	"github.com/vechain/thor/state"
	"github.com/vechain/thor/thor"
)

var log = log15.New("pkg", "optimizer")

const (
	propsStoreName = "optimizer.props"
	statusKey      = "status"

	minSpan = 1800  // 5 hours
	maxSpan = 18000 // 50 hours
)

// Optimizer is a background task to optimize tries.
type Optimizer struct {
	db     *muxdb.MuxDB
	repo   *chain.Repository
	ctx    context.Context
	cancel func()
	goes   co.Goes
	prune  bool
}

// New creates and starts the optimizer.
func New(db *muxdb.MuxDB, repo *chain.Repository, prune bool) *Optimizer {
	ctx, cancel := context.WithCancel(context.Background())
	o := &Optimizer{
		db:     db,
		repo:   repo,
		ctx:    ctx,
		cancel: cancel,
		prune:  prune,
	}
	o.goes.Go(func() {
		if err := o.loop(); err != nil {
			if err != context.Canceled {
				log.Warn("optimizer interrupted", "error", err)
			}
		}
	})
	return o
}

// Stop stops the optimizer.
func (p *Optimizer) Stop() {
	p.cancel()
	p.goes.Wait()
}

// loop is the main loop.
func (p *Optimizer) loop() error {
	log.Info("optimizer started")

	var (
		status      status
		lastLogTime = time.Now().UnixNano()
	)
	if err := status.Load(p.db); err != nil {
		return err
	}

	for {
		// select target
		target := p.repo.BestBlockSummary().Header.Number()
		if min, max := status.Base+minSpan, status.Base+maxSpan; target < min {
			target = min
		} else if target > max {
			target = max
		}

		steadyChain, err := p.waitUntilSteady(target)
		if err != nil {
			return err
		}
		startTime := time.Now().UnixNano()
		if err := p.alignToPartition(status.Base, target, func(alignedBase, alignedTarget uint32) error {
			summary, err := steadyChain.GetBlockSummary(alignedTarget)
			// no need to update leaf bank for index trie
			if p.prune {
				// prune the index trie
				if err != nil {
					return err
				}
				indexTrie := p.db.NewTrie(chain.IndexTrieName, summary.IndexRoot, summary.Header.Number())
				indexTrie.SetNoFillCache(true)
				if err := indexTrie.Prune(p.ctx, alignedBase); err != nil {
					return err
				}
			}
			// optimize storage tries
			return p.optimizeStorageTries(alignedBase, summary.Header)
		}); err != nil {
			return err
		}

		// optimize the account trie.
		if target > thor.MaxStateHistory {
			accountTarget := target - thor.MaxStateHistory
			if err := p.alignToPartition(status.AccountBase, accountTarget, func(alignedBase, alignedTarget uint32) error {
				header, err := steadyChain.GetBlockHeader(alignedTarget)
				if err != nil {
					return err
				}
				accTrie := p.db.NewTrie(state.AccountTrieName, header.StateRoot(), header.Number())
				accTrie.SetNoFillCache(true)
				if err := p.updateLeafBank(accTrie, alignedBase); err != nil {
					return err
				}
				if p.prune {
					if err := accTrie.Prune(p.ctx, alignedBase); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				return err
			}
			status.AccountBase = accountTarget + 1
		}
		if now := time.Now().UnixNano(); now-lastLogTime > int64(time.Second*20) {
			lastLogTime = now
			log.Info("optimized tries",
				"range", fmt.Sprintf("#%v+%v", status.Base, target-status.Base),
				"et", time.Duration(now-startTime),
			)
		}
		status.Base = target + 1
		if err := status.Save(p.db); err != nil {
			return err
		}
	}
}

func (p *Optimizer) optimizeStorageTries(base uint32, header *block.Header) error {
	accTrie := p.db.NewTrie(state.AccountTrieName, header.StateRoot(), header.Number())
	accTrie.SetNoFillCache(true)
	accIter := accTrie.NodeIterator(nil, base)

	// iterate updated accounts since the base
	for accIter.Next(true) {
		if leaf := accIter.Leaf(); leaf != nil {
			var acc state.Account
			if err := rlp.DecodeBytes(leaf.Value, &acc); err != nil {
				return err
			}
			if len(acc.StorageRoot) == 0 {
				// skip, no storage
				continue
			}

			var meta state.AccountMetadata
			if err := rlp.DecodeBytes(leaf.Meta, &meta); err != nil {
				return err
			}
			if meta.StorageCommitNum < base {
				// skip, no storage updates
				continue
			}
			sTrie := p.db.NewTrie(
				state.StorageTrieName(meta.Addr, meta.StorageInitCommitNum),
				thor.BytesToBytes32(acc.StorageRoot),
				meta.StorageCommitNum,
			)
			sTrie.SetNoFillCache(true)
			if err := p.updateLeafBank(sTrie, base); err != nil {
				return err
			}
			if p.prune {
				if err := sTrie.Prune(p.ctx, base); err != nil {
					return err
				}
			}
		}
	}
	return accIter.Error()
}

func (p *Optimizer) updateLeafBank(trie *muxdb.Trie, base uint32) error {
	return p.db.TrieLeafBank().Update(trie.Name(), trie.CommitNum(), func(save muxdb.TrieSaveLeaf) error {
		it := trie.NodeIterator(nil, base)
		for it.Next(true) {
			if leaf := it.Leaf(); leaf != nil {
				if err := save(it.LeafKey(), leaf); err != nil {
					return err
				}
			}
		}
		return it.Error()
	})
}

// alignToPartition aligns to deduped node partition. Aligned partition becomes a checkpoint.
func (p *Optimizer) alignToPartition(base, target uint32, cb func(alignedBase, alignedTarget uint32) error) error {
	for {
		pid := muxdb.TrieDedupedNodePartitionFactor.Which(base)
		_, limit := muxdb.TrieDedupedNodePartitionFactor.Range(pid)

		if limit > target {
			limit = target
		}

		if err := cb(base, limit); err != nil {
			return err
		}

		if target == limit {
			return nil
		}
		base = limit + 1
	}
}

func (p *Optimizer) getProposerCount(summary *chain.BlockSummary) (int, error) {
	st := state.New(p.db, summary.Header.StateRoot(), summary.Header.Number(), summary.SteadyNum)
	endorsement, err := builtin.Params.Native(st).Get(thor.KeyProposerEndorsement)
	if err != nil {
		return 0, err
	}

	candidates, err := builtin.Authority.Native(st).Candidates(endorsement, thor.MaxBlockProposers)
	if err != nil {
		return 0, err
	}
	return len(candidates), nil
}

// waitUntilSteady waits until the target block number becomes almost final(steady),
// and returns the steady chain.
func (p *Optimizer) waitUntilSteady(target uint32) (*chain.Chain, error) {
	// the knowned steady id is newer than target
	if steadyID := p.repo.SteadyBlockID(); block.Number(steadyID) >= target {
		return p.repo.NewChain(steadyID), nil
	}

	ticker := p.repo.NewTicker()
	for {
		best := p.repo.BestBlockSummary()
		// requires the best block number larger enough than target.
		if best.Header.Number() > target+uint32(thor.MaxBlockProposers) {
			proposerCount, err := p.getProposerCount(best)
			if err != nil {
				return nil, err
			}

			set := make(map[thor.Address]struct{})
			// reverse iterate the chain and collect signers.

			for header := best.Header; header.Number() >= target; {
				signer, _ := header.Signer()
				set[signer] = struct{}{}

				if len(set) >= (proposerCount+1)/2 {
					// got enough unique signers
					steadyID := header.ID()
					if err := p.repo.SetSteadyBlockID(steadyID); err != nil {
						return nil, err
					}
					return p.repo.NewChain(steadyID), nil
				}
				parent, err := p.repo.GetBlockSummary(header.ParentID())
				if err != nil {
					return nil, err
				}
				header = parent.Header
			}
		}

		select {
		case <-p.ctx.Done():
			return nil, p.ctx.Err()
		case <-ticker.C():
		}
	}
}
