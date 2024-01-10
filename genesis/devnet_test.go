package genesis_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/vechain/thor/v2/genesis"
	"github.com/vechain/thor/v2/thor"
)

// TestDevAccounts checks if DevAccounts function returns the expected number of accounts and initializes them correctly
func TestDevAccounts(t *testing.T) {
	accounts := genesis.DevAccounts()

	// Assuming 10 private keys are defined in DevAccounts
	expectedNumAccounts := 10
	assert.Equal(t, expectedNumAccounts, len(accounts), "Incorrect number of dev accounts returned")

	for _, account := range accounts {
		assert.NotNil(t, account.PrivateKey, "Private key should not be nil")
		assert.NotEqual(t, thor.Address{}, account.Address, "Account address should be valid")
	}
}

// TestNewDevnet checks if NewDevnet function returns a correctly initialized Genesis object
func TestNewDevnet(t *testing.T) {
	genesisObj := genesis.NewDevnet()

	assert.NotNil(t, genesisObj, "NewDevnet should return a non-nil Genesis object")
	assert.NotEqual(t, thor.Bytes32{}, genesisObj.ID(), "Genesis ID should be valid")
	assert.Equal(t, "devnet", genesisObj.Name(), "Genesis name should be 'devnet'")
}

// TestNewDevnetCustomTimestamp checks if NewDevnetCustomTimestamp function correctly uses a custom timestamp
func TestNewDevnetCustomTimestamp(t *testing.T) {
	customTimestamp := uint64(1600000000) // Example timestamp
	genesisObj := genesis.NewDevnetCustomTimestamp(customTimestamp)

	assert.NotNil(t, genesisObj, "NewDevnetCustomTimestamp should return a non-nil Genesis object")

	// Additional checks might be needed to verify if the custom timestamp is set correctly in the Genesis object
	// This depends on your implementation details
}
