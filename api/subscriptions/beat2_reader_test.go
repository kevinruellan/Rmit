// Copyright (c) 2024 The VeChainThor developers

// Distributed under the GNU Lesser General Public License v3.0 software license, see the accompanying
// file LICENSE or <https://www.gnu.org/licenses/lgpl-3.0.html>

package subscriptions

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/vechain/thor/v2/thor"
)

func TestBeat2Reader_Read(t *testing.T) {
	// Arrange
	repo, generatedBlocks, _ := initChain(t)
	genesisBlk := generatedBlocks[0]
	newBlock := generatedBlocks[1]

	// Act
	beatReader := newBeat2Reader(repo, genesisBlk.Header().ID())
	res, ok, err := beatReader.Read()

	// Assert
	assert.NoError(t, err)
	assert.True(t, ok)
	if beatMsg, ok := res[0].(*Beat2Message); !ok {
		t.Fatal("unexpected type")
	} else {
		assert.Equal(t, newBlock.Header().Number(), beatMsg.Number)
		assert.Equal(t, newBlock.Header().ID(), beatMsg.ID)
		assert.Equal(t, newBlock.Header().ParentID(), beatMsg.ParentID)
		assert.Equal(t, newBlock.Header().Timestamp(), beatMsg.Timestamp)
		assert.Equal(t, uint32(newBlock.Header().TxsFeatures()), beatMsg.TxsFeatures)
	}

	// Test case 2: There is no new block
	beatReader = newBeat2Reader(repo, newBlock.Header().ID())
	res, ok, err = beatReader.Read()

	assert.NoError(t, err)
	assert.False(t, ok)
	assert.Empty(t, res)

	// Test case 3: Error when reading blocks
	beatReader = newBeat2Reader(repo, thor.MustParseBytes32("0xffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"))
	res, ok, err = beatReader.Read()

	assert.Error(t, err)
	assert.False(t, ok)
	assert.Empty(t, res)
}
