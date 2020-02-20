// Copyright (c) 2018 The VeChainThor developers

// Distributed under the GNU Lesser General Public License v3.0 software license, see the accompanying
// file LICENSE or <https://www.gnu.org/licenses/lgpl-3.0.html>

package node

import (
	"context"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/event"
	"github.com/vechain/thor/block"
	"github.com/vechain/thor/comm"
	"github.com/vechain/thor/packer"
	"github.com/vechain/thor/thor"
	"github.com/vechain/thor/tx"
)

// packerLoop is executed by the leader and committee members to
// 1. prepare and broadcast block summary & tx set as leader
// 2. prepare and broadcast endorsements as committee members
// 3. pack and broadcast header as leader
// 4. pack and commit new block as leader
func (n *Node) packerLoop(ctx context.Context) {
	debugLog := func(str string, kv ...interface{}) {
		log.Info(str, append([]interface{}{"key", "packer"}, kv...)...)
	}

	errLog := func(msg string, kv ...interface{}) {
		log.Error(msg, append([]interface{}{"key", "packer"}, kv...)...)
	}

	debugLog("enter packer loop")
	defer debugLog("leave packer loop")

	debugLog("waiting for synchronization...")
	select {
	case <-ctx.Done():
		return
	case <-n.comm.Synced():
	}
	debugLog("synchronization process done")

	var (
		flow *packer.Flow
		// flow is activated if it packs bs and ts and is in its scheduled round
		err    error
		ticker = time.NewTicker(time.Second)

		maxTxAdoptDur = 3 // max number of seconds allowed to prepare transactions

		start, txSetBlockSummaryDone, endorsementDone, blockDone, commitDone mclock.AbsTime
	)

	defer ticker.Stop()

	n.packer.SetTargetGasLimit(n.targetGasLimit)

	var scope event.SubscriptionScope
	defer scope.Close()

	newEndorsementCh := make(chan *comm.NewEndorsementEvent)
	scope.Track(n.comm.SubscribeEndorsement(newEndorsementCh))

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := uint64(time.Now().Unix())
			best := n.repo.BestBlock()

			if flow == nil {
				// Schedule a round to be the leader
				flow, err = n.packer.Schedule(best.Header(), now)
				if err != nil {
					errLog("Schedule", "err", err)
					continue
				}
			}

			// Check whether the best block has changed
			if flow.ParentHeader().ID() != best.Header().ID() {
				flow = nil
				debugLog("re-schedule packer due to new best block")
				continue
			}

			// wait until the scheduled the time
			if now < flow.When() {
				continue
			}

			// if timeout, reset
			if now > flow.When()+thor.BlockInterval {
				flow = nil
				debugLog("re-schedule packer due to timeout")
				continue
			}

			// do nothing if having already packed bs and ts
			if flow.HasPackedBlockSummary() {
				continue
			}

			start = mclock.Now()

			bs, ts, err := n.packTxSetAndBlockSummary(flow, maxTxAdoptDur)
			if err != nil {
				flow = nil
				errLog("pack bs and ts", "err", err)
				continue
			}

			// activated = true // Mark the flow as activated

			txSetBlockSummaryDone = mclock.Now()

			// lbs = bs // save a local copy of the latest block summary

			debugLog("bs ==>", "id", bs.ID().Abev())
			// n.comm.BroadcastBlockSummary(bs)

			// send the new block summary to comm.newBlockSummaryCh to send it to housekeeping
			// and endorsor loops for broadcasting and endorsement, respectively. packer loop
			// does not take the responsibility of endorsing block summary
			n.comm.SendBlockSummaryToFeed(bs)

			if ts != nil {
				debugLog("ts ==>", "id", ts.ID().Abev())
				n.comm.BroadcastTxSet(ts)
			}

			// // test its committee membership and if elected, endorse the new block summary
			// ok, proof, err := n.cons.IsCommittee(n.master.VrfPrivateKey, now)
			// if err != nil {
			// 	errLog("check committee", "err", err)
			// 	continue
			// }
			// if ok {
			// 	// Endorse the block summary
			// 	ed := block.NewEndorsement(bs, proof)
			// 	sig, err := crypto.Sign(ed.SigningHash().Bytes(), n.master.PrivateKey)
			// 	if err != nil {
			// 		errLog("Signing endorsement", "err", err)
			// 		continue
			// 	}
			// 	ed = ed.WithSignature(sig)

			// 	if ok := flow.AddEndoresement(ed); !ok {
			// 		debugLog("Failed to add ed", "#", flow.NumOfEndorsements(), "id", ed.ID().Abev())
			// 	} else {
			// 		debugLog("Added ed", "#", flow.NumOfEndorsements(), "id", ed.ID().Abev())
			// 		debugLog("ed ==>", "id", ed.ID().Abev())
			// 		n.comm.BroadcastEndorsement(ed)
			// 	}
			// }

		case ev := <-newEndorsementCh:
			// proceed only when the node has already packed a block summary
			if flow == nil || !flow.HasPackedBlockSummary() {
				continue
			}

			best := n.repo.BestBlock()
			now := uint64(time.Now().Unix())

			// Check whether the best block has changed
			if flow.ParentHeader().ID() != best.Header().ID() {
				flow = nil
				debugLog("re-schedule packer due to new best block")
				continue
			}

			// Check timeout
			if now > flow.When()+thor.BlockInterval {
				flow = nil
				debugLog("re-schedule packer due to timeout")
				continue
			}

			ed := ev.Endorsement
			debugLog("<== ed", "id", ed.ID().Abev())

			if err := n.cons.ValidateEndorsement(ev.Endorsement, best.Header(), now); err != nil {
				debugLog("invalid ed", "err", err, "id", ed.ID().Abev())
				// fmt.Println(ed.String())
				continue
			}

			if ok := flow.AddEndoresement(ed); !ok {
				debugLog("Failed to add ed", "#", flow.NumOfEndorsements(), "id", ed.ID().Abev())
			} else {
				debugLog("Added ed", "#", flow.NumOfEndorsements(), "id", ed.ID().Abev())
			}

			// Pack a new block if there have been enough endorsements collected
			if uint64(flow.NumOfEndorsements()) < thor.CommitteeSize {
				continue
			}

			endorsementDone = mclock.Now()

			debugLog("Packing new header")
			newBlock, stage, receipts, err := flow.Pack(n.master.PrivateKey)
			if err != nil {
				errLog("pack block", "err", err)
				flow = nil
				continue
			}
			blockDone = mclock.Now()

			// reset flow
			flow = nil

			debugLog("Committing new block")
			if _, err := stage.Commit(); err != nil {
				errLog("commit state", "err", err)
				flow = nil
				continue
			}

			prevTrunk, curTrunk, err := n.commitBlock(newBlock, receipts)
			if err != nil {
				errLog("commit block", "err", err)
				flow = nil
				continue
			}
			commitDone = mclock.Now()

			n.processFork(prevTrunk, curTrunk)

			if prevTrunk.HeadID() != curTrunk.HeadID() {
				debugLog("hd ==>", "id", newBlock.Header().ID().Abev())
				n.comm.BroadcastBlockHeader(newBlock.Header())

				display(newBlock, receipts,
					txSetBlockSummaryDone-start,
					endorsementDone-txSetBlockSummaryDone,
					blockDone-endorsementDone,
					commitDone-blockDone,
				)
			}

			if v, updated := n.bandwidth.Update(newBlock.Header(), time.Duration(commitDone-start)); updated {
				log.Debug("bandwidth updated", "gps", v)
			}
		}
	}
}

func (n *Node) packTxSetAndBlockSummary(flow *packer.Flow, maxTxPackingDur int) (*block.Summary, *block.TxSet, error) {
	var txsToRemove []*tx.Transaction
	defer func() {
		for _, tx := range txsToRemove {
			n.txPool.Remove(tx.Hash(), tx.ID())
		}
	}()

	done := make(chan struct{})
	go func() {
		time.Sleep(time.Duration(maxTxPackingDur) * time.Second)
		done <- struct{}{}
	}()

	for _, tx := range n.txPool.Executables() {
		select {
		case <-done:
			// debugLog("Leave tx adopting loop", "Iter", i)
			break
		default:
		}
		// debugLog("Adopting tx", "txid", tx.ID())
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

	bs, ts, err := flow.PackTxSetAndBlockSummary(n.master.PrivateKey)
	if err != nil {
		return nil, nil, err
	}

	return bs, ts, nil
}

func display(blk *block.Block, receipts tx.Receipts, prepareElapsed, collectElapsed, packElapsed, commitElapsed mclock.AbsTime) {
	blockID := blk.Header().ID()
	log.Info("📦 new block packed",
		"txs", len(receipts),
		"mgas", float64(blk.Header().GasUsed())/1000/1000,
		"et", fmt.Sprintf("%v|%v|%v|%v",
			common.PrettyDuration(prepareElapsed),
			common.PrettyDuration(collectElapsed),
			common.PrettyDuration(packElapsed),
			common.PrettyDuration(commitElapsed),
		),
		"id", fmt.Sprintf("[#%v…%x]", block.Number(blockID), blockID[28:]),
	)
}
