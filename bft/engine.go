// Copyright (c) 2022 The VeChainThor developers

// Distributed under the GNU Lesser General Public License v3.0 software license, see the accompanying
// file LICENSE or <https://www.gnu.org/licenses/lgpl-3.0.html>
package bft

import (
	"sort"
	"sync/atomic"

	lru "github.com/hashicorp/golang-lru"
	"github.com/pkg/errors"
	"github.com/vechain/thor/block"
	"github.com/vechain/thor/builtin"
	"github.com/vechain/thor/cache"
	"github.com/vechain/thor/chain"
	"github.com/vechain/thor/kv"
	"github.com/vechain/thor/muxdb"
	"github.com/vechain/thor/state"
	"github.com/vechain/thor/thor"
)

const dataStoreName = "bft.engine"

var finalizedKey = []byte("finalized")

// BFTEngine tracks all votes of blocks, computes the finalized checkpoint.
// Not thread-safe!
type BFTEngine struct {
	repo       *chain.Repository
	data       kv.Store
	stater     *state.Stater
	forkConfig thor.ForkConfig
	master     thor.Address
	casts      casts
	finalized  atomic.Value
	caches     struct {
		state     *lru.Cache
		quality   *lru.Cache
		mbp       *lru.Cache
		justifier *cache.PrioCache
	}
}

// NewEngine creates a new bft engine.
func NewEngine(repo *chain.Repository, mainDB *muxdb.MuxDB, forkConfig thor.ForkConfig, master thor.Address) (*BFTEngine, error) {
	engine := BFTEngine{
		repo:       repo,
		data:       mainDB.NewStore(dataStoreName),
		stater:     state.NewStater(mainDB),
		forkConfig: forkConfig,
		master:     master,
	}

	engine.caches.state, _ = lru.New(1024)
	engine.caches.quality, _ = lru.New(1024)
	engine.caches.mbp, _ = lru.New(8)
	engine.caches.justifier = cache.NewPrioCache(16)

	if val, err := engine.data.Get(finalizedKey); err != nil {
		if !engine.data.IsNotFound(err) {
			return nil, err
		}
		engine.finalized.Store(engine.repo.GenesisBlock().Header().ID())
	} else {
		engine.finalized.Store(thor.BytesToBytes32(val))
	}

	return &engine, nil
}

// Finalized returns the finalized checkpoint.
func (engine *BFTEngine) Finalized() thor.Bytes32 {
	return engine.finalized.Load().(thor.Bytes32)
}

// Accepts checks if the given block is on the same branch of finalized checkpoint.
func (engine *BFTEngine) Accepts(parentID thor.Bytes32) (bool, error) {
	finalized := engine.Finalized()

	if block.Number(finalized) != 0 {
		if included, err := engine.repo.NewChain(parentID).HasBlock(finalized); err != nil {
			return false, err
		} else if !included {
			return false, nil
		}
	}

	return true, nil
}

// Select selects between the new block and the current best, return true if new one is better.
func (engine *BFTEngine) Select(header *block.Header) (bool, error) {
	newSt, err := engine.computeState(header)
	if err != nil {
		return false, err
	}

	best := engine.repo.BestBlockSummary().Header
	bestSt, err := engine.computeState(best)
	if err != nil {
		return false, err
	}

	if newSt.Quality != bestSt.Quality {
		return newSt.Quality > bestSt.Quality, nil
	}

	return header.BetterThan(best), nil
}

// CommitBlock commits bft state to storage.
func (engine *BFTEngine) CommitBlock(header *block.Header, isPacking bool) error {
	state, err := engine.computeState(header)
	if err != nil {
		return nil
	}

	blockNum := header.Number()
	// save quality if needed
	if getStorePoint(blockNum) == blockNum {
		if err := saveQuality(engine.data, header.ID(), state.Quality); err != nil {
			return err
		}
		engine.caches.quality.Add(header.ID(), state.Quality)
	}

	// update finalized if new block commits this round
	if state.CommitAt != nil && header.ID() == *state.CommitAt && state.Quality > 1 {
		id, err := engine.findCheckpointByQuality(state.Quality-1, engine.Finalized(), header.ID())
		if err != nil {
			return err
		}

		if err := engine.data.Put(finalizedKey, id[:]); err != nil {
			return err
		}
		engine.finalized.Store(id)
	}

	// mark voted if packing
	if isPacking {
		checkpoint, err := engine.repo.NewChain(header.ID()).GetBlockID(getCheckPoint(blockNum))
		if err != nil {
			return err
		}
		engine.casts.Mark(checkpoint, state.Quality)
	}

	return nil
}

// GetVote computes the vote for a given parent block ID.
// Packer only.
func (engine *BFTEngine) GetVote(parentID thor.Bytes32) (v block.Vote, err error) {
	// laze init casts
	if engine.casts == nil {
		if engine.casts, err = newCasts(engine); err != nil {
			return
		}
	}

	sum, err := engine.repo.GetBlockSummary(parentID)
	if err != nil {
		return
	}

	st, err := engine.computeState(sum.Header)
	if err != nil {
		return
	}

	if st.Quality == 0 {
		return
	}

	finalized := engine.Finalized()

	// most recent justified checkpoint
	var recentJC thor.Bytes32
	theQuality := st.Quality
	if st.Justified {
		// if justified in this round, use this round's checkpoint
		checkpoint, err := engine.repo.NewChain(parentID).GetBlockID(getCheckPoint(block.Number(parentID)))
		if err != nil {
			return block.WIT, err
		}
		recentJC = checkpoint
	} else {
		checkpoint, err := engine.findCheckpointByQuality(theQuality, finalized, parentID)
		if err != nil {
			return block.WIT, err
		}
		recentJC = checkpoint
	}

	// see https://github.com/vechain/VIPs/blob/master/vips/VIP-220.md
	for _, cast := range engine.casts.Slice(engine.Finalized()) {
		x, y := recentJC, cast.checkpoint
		if block.Number(cast.checkpoint) > block.Number(recentJC) {
			x, y = cast.checkpoint, recentJC
		}

		// comparing packer's vote with the most recent JC, if packer voted
		// a conflict checkpoint at (recentJC.Quality or -1) vote WIT
		if includes, err := engine.repo.NewChain(x).HasBlock(y); err != nil {
			return block.WIT, err
		} else if !includes && cast.quality >= theQuality-1 {
			return block.WIT, nil
		}
	}

	return block.COM, nil
}

// computeState computes the bft state regarding the given block header.
func (engine *BFTEngine) computeState(header *block.Header) (*bftState, error) {
	if cached, ok := engine.caches.state.Get(header.ID()); ok {
		return cached.(*bftState), nil
	}

	if header.Number() == 0 || header.Number() < engine.forkConfig.FINALITY {
		return &bftState{}, nil
	}

	var (
		js  *justifier
		end uint32
	)

	if entry := engine.caches.justifier.Remove(header.ParentID()); !isCheckPoint(header.Number()) && entry != nil {
		js = interface{}(entry.Entry.Value).(*justifier)
		end = header.Number() - 1
	} else {
		// create a new vote set if cache missed or new block is checkpoint
		var err error
		js, err = newJustifier(engine, header.ParentID())
		if err != nil {
			return nil, errors.Wrap(err, "failed to create vote set")
		}
		end = js.checkpoint
	}

	h := header
	for {
		if js.isCommitted() || h.Vote() == nil {
			break
		}

		signer, err := h.Signer()
		if err != nil {
			return nil, err
		}

		js.AddBlock(h.ID(), signer, *h.Vote())

		if h.Number() <= end {
			break
		}

		sum, err := engine.repo.GetBlockSummary(h.ParentID())
		if err != nil {
			return nil, err
		}
		h = sum.Header
	}

	st := js.Summarize()

	engine.caches.state.Add(header.ID(), st)
	engine.caches.justifier.Set(header.ID(), js, float64(header.Number()))
	return st, nil
}

// findCheckpointByQuality finds the first checkpoint reaches the given quality.
func (engine *BFTEngine) findCheckpointByQuality(target uint32, finalized, parentID thor.Bytes32) (blockID thor.Bytes32, err error) {
	defer func() {
		if e := recover(); e != nil {
			err = e.(error)
			return
		}
	}()

	searchStart := block.Number(finalized)
	if searchStart == 0 {
		searchStart = getCheckPoint(engine.forkConfig.FINALITY)
	}

	c := engine.repo.NewChain(parentID)
	get := func(i int) (uint32, error) {
		id, err := c.GetBlockID(getStorePoint(searchStart + uint32(i)*thor.CheckpointInterval))
		if err != nil {
			return 0, err
		}
		return engine.getQuality(id)
	}

	n := int((block.Number(parentID) + 1 - searchStart) / thor.CheckpointInterval)
	num := sort.Search(n, func(i int) bool {
		quality, err := get(i)
		if err != nil {
			panic(err)
		}

		return quality >= target
	})

	if num == n {
		return thor.Bytes32{}, errors.New("failed find the block by quality")
	}

	quality, err := get(num)
	if err != nil {
		return thor.Bytes32{}, err
	}

	if quality != target {
		return thor.Bytes32{}, errors.New("failed to find the block by quality")
	}

	return c.GetBlockID(searchStart + uint32(num)*thor.CheckpointInterval)
}

func (engine *BFTEngine) getMaxBlockProposers(sum *chain.BlockSummary) (mbp uint64, err error) {
	if cached, ok := engine.caches.mbp.Get(sum.Header.ID()); ok {
		return cached.(uint64), nil
	}

	defer func() {
		if err == nil {
			engine.caches.mbp.Add(sum.Header.ID(), mbp)
		}
	}()

	state := engine.stater.NewState(sum.Header.StateRoot(), sum.Header.Number(), sum.Conflicts, sum.SteadyNum)
	params, err := builtin.Params.Native(state).Get(thor.KeyMaxBlockProposers)
	if err != nil {
		return
	}
	mbp = params.Uint64()
	if mbp == 0 || mbp > thor.InitialMaxBlockProposers {
		mbp = thor.InitialMaxBlockProposers
	}

	return
}

func (engine *BFTEngine) getQuality(id thor.Bytes32) (quality uint32, err error) {
	if cached, ok := engine.caches.quality.Get(id); ok {
		return cached.(uint32), nil
	}

	defer func() {
		if err == nil {
			engine.caches.quality.Add(id, quality)
		}
	}()

	return loadQuality(engine.data, id)
}

func getCheckPoint(blockNum uint32) uint32 {
	return blockNum / thor.CheckpointInterval * thor.CheckpointInterval
}

func isCheckPoint(blockNum uint32) bool {
	return getCheckPoint(blockNum) == blockNum
}

// save quality at the end of round
func getStorePoint(blockNum uint32) uint32 {
	return getCheckPoint(blockNum) + thor.CheckpointInterval - 1
}
