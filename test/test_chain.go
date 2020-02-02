// Copyright (c) 2018 The VeChainThor developers

// Distributed under the GNU Lesser General Public License v3.0 software license, see the accompanying
// file LICENSE or <https://www.gnu.org/licenses/lgpl-3.0.html>

package test

import (
	"crypto/ecdsa"
	"errors"
	"math/big"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/vechain/thor/block"
	"github.com/vechain/thor/builtin"
	"github.com/vechain/thor/chain"
	"github.com/vechain/thor/consensus"
	"github.com/vechain/thor/genesis"
	"github.com/vechain/thor/lvldb"
	"github.com/vechain/thor/packer"
	"github.com/vechain/thor/state"
	"github.com/vechain/thor/thor"
	"github.com/vechain/thor/tx"
	"github.com/vechain/thor/vrf"
)

// TxBuilder ...
func TxBuilder(tag byte) *tx.Builder {
	address := thor.BytesToAddress([]byte("addr"))
	return new(tx.Builder).
		GasPriceCoef(1).
		Gas(1000000).
		Expiration(100).
		Clause(tx.NewClause(&address).WithValue(big.NewInt(10)).WithData(nil)).
		Nonce(1).
		ChainTag(tag)
}

// TxSign ...
func TxSign(builder *tx.Builder, sk *ecdsa.PrivateKey) *tx.Transaction {
	transaction := builder.Build()
	sig, _ := crypto.Sign(transaction.SigningHash().Bytes(), sk)
	return transaction.WithSignature(sig)
}

// TempChain ...
type TempChain struct {
	Con          *consensus.Consensus
	Time         uint64
	Tag          byte
	Original     *block.Block
	Stage        *state.Stage
	Receipts     tx.Receipts
	Proposer     *account
	Parent       *block.Block
	Nodes        []*account
	GenesisBlock *block.Block
	Chain        *chain.Chain
	StateCreator *state.Creator
	forkConfig   thor.ForkConfig
}

type account struct {
	ethsk *ecdsa.PrivateKey
	addr  thor.Address
	vrfsk *vrf.PrivateKey
	vrfpk *vrf.PublicKey
}

// NewTempChain generates thor.MaxBlockProposers key pairs and register them as master nodes
func NewTempChain(forkConfig thor.ForkConfig) (*TempChain, error) {
	db, err := lvldb.NewMem()
	if err != nil {
		return nil, err
	}

	var accs []*account
	for i := uint64(0); i < thor.MaxBlockProposers; i++ {
		ethsk, _ := crypto.GenerateKey()
		addr := crypto.PubkeyToAddress(ethsk.PublicKey)
		vrfpk, vrfsk := vrf.GenKeyPair()
		accs = append(accs, &account{ethsk, thor.BytesToAddress(addr.Bytes()), vrfsk, vrfpk})
	}

	launchTime := uint64(1526400000)
	gen := new(genesis.Builder).
		GasLimit(thor.InitialGasLimit).
		Timestamp(launchTime).
		State(func(state *state.State) error {
			state.SetCode(builtin.Authority.Address, builtin.Authority.RuntimeBytecodes())
			state.SetCode(builtin.Energy.Address, builtin.Energy.RuntimeBytecodes())
			state.SetCode(builtin.Params.Address, builtin.Params.RuntimeBytecodes())
			state.SetCode(builtin.Prototype.Address, builtin.Prototype.RuntimeBytecodes())
			state.SetCode(builtin.Extension.Address, builtin.Extension.RuntimeBytecodes())

			builtin.Params.Native(state).Set(thor.KeyExecutorAddress, new(big.Int).SetBytes(genesis.DevAccounts()[0].Address[:]))

			bal, _ := new(big.Int).SetString("1000000000000000000000000000", 10)
			for _, acc := range accs {
				state.SetBalance(acc.addr, bal)
				state.SetEnergy(acc.addr, bal, launchTime)

				builtin.Authority.Native(state).Add(acc.addr, acc.addr, thor.Bytes32{}, acc.vrfpk.Bytes32())
			}
			return nil
		})

	stateCreator := state.NewCreator(db)
	genesisBlock, _, err := gen.Build(stateCreator)
	if err != nil {
		return nil, err
	}

	c, err := chain.New(db, genesisBlock)
	if err != nil {
		return nil, err
	}

	con := consensus.New(c, stateCreator, forkConfig)

	return &TempChain{
		Con:          con,
		Nodes:        accs,
		Tag:          c.Tag(),
		Chain:        c,
		StateCreator: stateCreator,
		GenesisBlock: genesisBlock,
		forkConfig:   forkConfig,
	}, nil
}

// NewBlock create a new block without committing to the state
func (tc *TempChain) NewBlock(round uint32, txs []*tx.Transaction) error {
	var (
		flow     *packer.Flow
		proposer *account
		err      error
	)

	now := tc.Con.Timestamp(round)
	parent := tc.Chain.BestBlock()

	if now < parent.Header().Timestamp() {
		return errors.New("new block earlier than the best block")
	}

	// search for the legit proposer
	for _, acc := range tc.Nodes {
		p := packer.New(tc.Chain, tc.StateCreator, acc.addr, &acc.addr, tc.forkConfig)
		flow, err = p.Schedule(parent.Header(), now)
		if err != nil {
			continue
		}

		if flow.When() == now {
			proposer = acc
			break
		}
		flow = nil
	}
	if flow == nil {
		return errors.New("No proposer found")
	}

	// add transactions
	for _, tx := range txs {
		flow.Adopt(tx)
	}

	// pack block summary
	bs, _, err := flow.PackTxSetAndBlockSummary(proposer.ethsk)
	if err != nil {
		return err
	}

	// pack endorsements
	for _, acc := range tc.Nodes {
		if ok, proof, _ := tc.Con.IsCommittee(acc.vrfsk, now); ok {
			ed := block.NewEndorsement(bs, proof)
			sig, _ := crypto.Sign(ed.SigningHash().Bytes(), acc.ethsk)
			ed = ed.WithSignature(sig)
			flow.AddEndoresement(ed)
		}
		if uint64(flow.NumOfEndorsements()) >= thor.CommitteeSize {
			break
		}
	}
	if uint64(flow.NumOfEndorsements()) < thor.CommitteeSize {
		return errors.New("Not enough endorsements added")
	}

	// pack block
	newBlock, stage, receipts, err := flow.Pack(proposer.ethsk)
	if err != nil {
		return err
	}

	// validate block
	if _, _, err := tc.Con.Process(newBlock, flow.When()); err != nil {
		return err
	}

	tc.Parent = parent
	tc.Time = now
	tc.Original = newBlock
	tc.Proposer = proposer
	tc.Stage = stage
	tc.Receipts = receipts

	return nil
}

// CommitNewBlock ...
func (tc *TempChain) CommitNewBlock() error {
	if _, err := tc.Chain.GetBlockHeader(tc.Original.Header().ID()); err == nil {
		return errors.New("known in-chain block")
	}

	if _, err := tc.Stage.Commit(); err != nil {
		return err
	}

	if _, err := tc.Chain.AddBlock(tc.Original, tc.Receipts); err != nil {
		return err
	}

	return nil
}

// Sign ...
func (tc *TempChain) Sign(blk *block.Block) (*block.Block, error) {
	sig, err := crypto.Sign(blk.Header().SigningHash().Bytes(), tc.Proposer.ethsk)
	if err != nil {
		return nil, err
	}
	return blk.WithSignature(sig), nil
}

// Rebuild ...
/**
 * rebuild takes the current block builder and re-compute the block summary
 * and the endorsements. It then update the builder with the correct
 * signatures and vrf proofs
 */
func (tc *TempChain) Rebuild(builder *block.Builder) (*block.Builder, error) {
	blk := builder.Build()
	header := blk.Header()

	// rebuild block summary
	bs := block.NewBlockSummary(
		header.ParentID(),
		header.TxsRoot(),
		header.Timestamp(),
		header.TotalScore())
	sig, err := crypto.Sign(bs.SigningHash().Bytes(), tc.Proposer.ethsk)
	if err != nil {
		return nil, err
	}
	bs = bs.WithSignature(sig)

	var (
		sigs   [][]byte
		proofs []*vrf.Proof
		N      = int(thor.CommitteeSize)
	)

	// rebuild endorsements
	for _, acc := range tc.Nodes {
		if ok, proof, err := tc.Con.IsCommittee(acc.vrfsk, header.Timestamp()); ok {
			ed := block.NewEndorsement(bs, proof)
			sig, _ := crypto.Sign(ed.SigningHash().Bytes(), acc.ethsk)
			proofs = append(proofs, proof)
			sigs = append(sigs, sig)
		} else if err != nil {
			return nil, err
		}
		if len(proofs) >= N {
			break
		}
	}
	if len(sigs) != N {
		return nil, errors.New("Not enough endorsements collected")
	}

	newBuilder := new(block.Builder).
		ParentID(header.ParentID()).
		Timestamp(header.Timestamp()).
		TotalScore(header.TotalScore()).
		GasLimit(header.GasLimit()).
		GasUsed(header.GasUsed()).
		Beneficiary(header.Beneficiary()).
		StateRoot(header.StateRoot()).
		ReceiptsRoot(header.ReceiptsRoot()).
		TransactionFeatures(header.TxsFeatures()).
		// update signatures and vrf proofs
		SigOnBlockSummary(sig).
		SigsOnEndorsement(sigs).
		VrfProofs(proofs)

	// add existing transactions
	for _, tx := range blk.Transactions() {
		newBuilder.Transaction(tx)
	}

	return newBuilder, nil
}

// OriginalBuilder ...
func (tc *TempChain) OriginalBuilder() *block.Builder {
	header := tc.Original.Header()
	return new(block.Builder).
		ParentID(header.ParentID()).
		Timestamp(header.Timestamp()).
		TotalScore(header.TotalScore()).
		GasLimit(header.GasLimit()).
		GasUsed(header.GasUsed()).
		Beneficiary(header.Beneficiary()).
		StateRoot(header.StateRoot()).
		ReceiptsRoot(header.ReceiptsRoot()).
		TransactionFeatures(header.TxsFeatures()).
		SigOnBlockSummary(header.SigOnBlockSummary()).
		SigsOnEndorsement(header.SigsOnEndoresment()).
		VrfProofs(header.VrfProofs())
}
