// Copyright (c) 2018 The VeChainThor developers

// Distributed under the GNU Lesser General Public License v3.0 software license, see the accompanying
// file LICENSE or <https://www.gnu.org/licenses/lgpl-3.0.html>

package solo

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/inconshreveable/log15"
	"github.com/pkg/errors"
	"github.com/vechain/thor/block"
	"github.com/vechain/thor/chain"
	"github.com/vechain/thor/co"
	"github.com/vechain/thor/consensus"
	"github.com/vechain/thor/genesis"
	"github.com/vechain/thor/logdb"
	"github.com/vechain/thor/packer"
	"github.com/vechain/thor/state"
	"github.com/vechain/thor/thor"
	"github.com/vechain/thor/tx"
	"github.com/vechain/thor/txpool"
	"github.com/vechain/thor/vrf"
)

var log = log15.New()

// Solo mode is the standalone client without p2p server
type Solo struct {
	chain       *chain.Chain
	txPool      *txpool.TxPool
	packer      *packer.Packer
	logDB       *logdb.LogDB
	bestBlockCh chan *block.Block
	gasLimit    uint64
	onDemand    bool

	cons *consensus.Consensus
	sk   *ecdsa.PrivateKey
}

// New returns Solo instance
func New(
	chain *chain.Chain,
	stateCreator *state.Creator,
	logDB *logdb.LogDB,
	txPool *txpool.TxPool,
	gasLimit uint64,
	onDemand bool,
	forkConfig thor.ForkConfig,
) *Solo {
	return &Solo{
		chain:  chain,
		txPool: txPool,
		packer: packer.New(
			chain,
			stateCreator,
			genesis.DevAccounts()[0].Address,
			&genesis.DevAccounts()[0].Address,
			forkConfig),
		logDB:    logDB,
		gasLimit: gasLimit,
		onDemand: onDemand,
		cons:     consensus.New(chain, stateCreator, forkConfig),
	}
}

// Run runs the packer for solo
func (s *Solo) Run(ctx context.Context) error {
	goes := &co.Goes{}

	defer func() {
		<-ctx.Done()
		goes.Wait()
	}()

	goes.Go(func() {
		s.loop(ctx)
	})

	log.Info("prepared to pack block")

	return nil
}

func (s *Solo) loop(ctx context.Context) {
	ticker := time.NewTicker(time.Duration(10) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info("stopping packing service......")
			return

		case <-ticker.C:
			log.Debug("START")
			startTime := mclock.Now()

			var (
				bs   *block.Summary
				ts   *block.TxSet
				err  error
				flow *packer.Flow
				done chan struct{}

				header   *block.Header
				stage    *state.Stage
				receipts tx.Receipts
			)

			best := s.chain.BestBlock()
			txs := s.txPool.Executables()
			flow, err = s.packer.Mock(best.Header(), uint64(time.Now().Unix()), s.gasLimit)
			if err != nil {
				log.Error("packer.Mock", "error", err)
			}

			done = make(chan struct{})
			go func() {
				time.Sleep(time.Duration(3) * time.Second)
				// close(done)
				done <- struct{}{}
			}()
			bs, ts, err = s.packTxSetAndBlockSummary(done, flow, txs)
			if err != nil {
				log.Debug("packTxSetAndBlockSummary", "error", err)
			}

			prepareElapsed := mclock.Now() - startTime

			// log.Debug("Endorsing starts")
			done = make(chan struct{})
			edCh := make(chan *block.Endorsement, thor.CommitteeSize)
			for i := uint64(0); i < thor.CommitteeSize*2; i++ {
				go s.endorse(done, edCh, bs)
			}

			var eds block.Endorsements
			for i := uint64(0); i < thor.CommitteeSize*2; i++ {
				if eds.Len() >= int(thor.CommitteeSize) {
					// close(done)
					done <- struct{}{}
					break
				}
				select {
				case ed := <-edCh:
					if eds.Add(ed) {
						// log.Debug("Collecting endorsement", "#endorsement", eds.Len())
					}
				}
			}
			// log.Debug("Endorsing ends")

			header, stage, receipts, err = flow.PackHeader(
				genesis.DevAccounts()[0].PrivateKey,
				eds.VrfProofs(),
				bs.Signature(),
				eds.Signatures(),
			)
			if err != nil {
				log.Error("flow.PackHeader", "error", err)
			}

			execElapsed := mclock.Now() - startTime

			b := block.Compose(header, ts.Transactions())
			err = s.commit(b, stage, receipts)
			if err != nil {
				log.Error("s.pack", "error", err)
			}

			commitElapsed := mclock.Now() - execElapsed - startTime

			display(b, receipts, prepareElapsed, execElapsed, commitElapsed)
		}
	}
}

func display(b *block.Block, receipts tx.Receipts, prepareElapsed, execElapsed, commitElapsed mclock.AbsTime) {
	blockID := b.Header().ID()
	log.Info("📦 new block packed",
		"txs", len(receipts),
		"mgas", float64(b.Header().GasUsed())/1000/1000,
		"et", fmt.Sprintf("%v|%v|%v", common.PrettyDuration(prepareElapsed), common.PrettyDuration(execElapsed), common.PrettyDuration(commitElapsed)),
		"id", fmt.Sprintf("[#%v…%x]", block.Number(blockID), blockID[28:]),
	)
	log.Debug(b.String())
}

func (s *Solo) commit(b *block.Block, stage *state.Stage, receipts tx.Receipts) error {
	if _, err := stage.Commit(); err != nil {
		return errors.WithMessage(err, "commit state")
	}

	// ignore fork when solo
	_, err := s.chain.AddBlock(b, receipts)
	if err != nil {
		return errors.WithMessage(err, "commit block")
	}

	task := s.logDB.NewTask().ForBlock(b.Header())
	for i, tx := range b.Transactions() {
		origin, _ := tx.Origin()
		task.Write(tx.ID(), origin, receipts[i].Outputs)
	}
	if err := task.Commit(); err != nil {
		return errors.WithMessage(err, "commit log")
	}

	return nil
}

func (s *Solo) endorse(done chan struct{}, edCh chan *block.Endorsement, bs *block.Summary) {
	endorseHash := bs.EndorseHash()

	for i := uint64(0); i < thor.CommitteeSize*2; i++ {
		_, sk := vrf.GenKeyPair()
		proof, _ := sk.Prove(endorseHash.Bytes())
		if consensus.IsCommitteeByProof(proof) {
			ed := block.NewEndorsement(bs, proof)
			sig, _ := crypto.Sign(ed.SigningHash().Bytes(), genesis.DevAccounts()[0].PrivateKey)
			ed = ed.WithSignature(sig)

			select {
			case <-done:
				// log.Debug("endorsement done")
				return
			case edCh <- ed:
				// log.Debug("endorsement sent", "id", hex.EncodeToString(ed.SigningHash().Bytes()))
				return
			}
		}
	}
}

func (s *Solo) packTxSetAndBlockSummary(done chan struct{}, flow *packer.Flow, txs tx.Transactions) (*block.Summary, *block.TxSet, error) {
	best := s.chain.BestBlock()

	var txsToRemove []*tx.Transaction
	defer func() {
		for _, tx := range txsToRemove {
			s.txPool.Remove(tx.Hash(), tx.ID())
		}
	}()
	for _, tx := range txs {
		select {
		case <-done:
			// log.Debug("Leave tx adopting loop", "Iter", i)
			break
		default:
		}
		// log.Debug("Adopting tx", "txid", tx.ID())
		err := flow.Adopt(tx)
		switch {
		case packer.IsGasLimitReached(err):
			break
		case packer.IsTxNotAdoptableNow(err):
			continue
		default:
			txsToRemove = append(txsToRemove, tx)
		}
	}

	ts := block.NewTxSet(flow.Txs())
	sig, err := crypto.Sign(ts.SigningHash().Bytes(), genesis.DevAccounts()[0].PrivateKey)
	if err != nil {
		return nil, nil, err
	}
	ts = ts.WithSignature(sig)

	parent := best.Header().ID()
	root := flow.Txs().RootHash()
	time := best.Header().Timestamp() + thor.BlockInterval
	bs := block.NewBlockSummary(parent, root, time)
	sig, err = crypto.Sign(bs.SigningHash().Bytes(), genesis.DevAccounts()[0].PrivateKey)
	if err != nil {
		return nil, nil, err
	}
	bs = bs.WithSignature(sig)
	// log.Debug(fmt.Sprintf("bs sig = 0x%x", bs.Signature()))

	return bs, ts, nil
}

// func (s *Solo) packing(pendingTxs tx.Transactions) error {
// 	best := s.chain.BestBlock()
// 	var txsToRemove []*tx.Transaction
// 	defer func() {
// 		for _, tx := range txsToRemove {
// 			s.txPool.Remove(tx.Hash(), tx.ID())
// 		}
// 	}()

// 	flow, err := s.packer.Mock(best.Header(), uint64(time.Now().Unix()), s.gasLimit)
// 	if err != nil {
// 		return errors.WithMessage(err, "mock packer")
// 	}

// 	startTime := mclock.Now()
// 	for _, tx := range pendingTxs {
// 		err := flow.Adopt(tx)
// 		switch {
// 		case packer.IsGasLimitReached(err):
// 			break
// 		case packer.IsTxNotAdoptableNow(err):
// 			continue
// 		default:
// 			txsToRemove = append(txsToRemove, tx)
// 		}
// 	}

// 	b, stage, receipts, err := flow.Pack(genesis.DevAccounts()[0].PrivateKey)
// 	if err != nil {
// 		return errors.WithMessage(err, "pack")
// 	}
// 	execElapsed := mclock.Now() - startTime

// 	// If there is no tx packed in the on-demand mode then skip
// 	if s.onDemand && len(b.Transactions()) == 0 {
// 		return nil
// 	}

// 	if _, err := stage.Commit(); err != nil {
// 		return errors.WithMessage(err, "commit state")
// 	}

// 	// ignore fork when solo
// 	_, err = s.chain.AddBlock(b, receipts)
// 	if err != nil {
// 		return errors.WithMessage(err, "commit block")
// 	}

// 	task := s.logDB.NewTask().ForBlock(b.Header())
// 	for i, tx := range b.Transactions() {
// 		origin, _ := tx.Origin()
// 		task.Write(tx.ID(), origin, receipts[i].Outputs)
// 	}
// 	if err := task.Commit(); err != nil {
// 		return errors.WithMessage(err, "commit log")
// 	}

// 	commitElapsed := mclock.Now() - startTime - execElapsed

// 	blockID := b.Header().ID()
// 	log.Info("📦 new block packed",
// 		"txs", len(receipts),
// 		"mgas", float64(b.Header().GasUsed())/1000/1000,
// 		"et", fmt.Sprintf("%v|%v", common.PrettyDuration(execElapsed), common.PrettyDuration(commitElapsed)),
// 		"id", fmt.Sprintf("[#%v…%x]", block.Number(blockID), blockID[28:]),
// 	)
// 	log.Debug(b.String())

// 	return nil
// }
