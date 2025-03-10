// Copyright (C) 2019-2022 Algorand, Inc.
// This file is part of go-algorand
//
// go-algorand is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// go-algorand is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with go-algorand.  If not, see <https://www.gnu.org/licenses/>.

package internal

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	ledgertesting "github.com/algorand/go-algorand/ledger/testing"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

type mockLedger struct {
	balanceMap map[basics.Address]basics.AccountData
	blocks     map[basics.Round]bookkeeping.BlockHeader
	blockErr   map[basics.Round]error
}

func (ml *mockLedger) lookup(addr basics.Address) (ledgercore.AccountData, error) {
	return ledgercore.ToAccountData(ml.balanceMap[addr]), nil
}

func (ml *mockLedger) lookupAppParams(addr basics.Address, aidx basics.AppIndex, cacheOnly bool) (ledgercore.AppParamsDelta, bool, error) {
	params, ok := ml.balanceMap[addr].AppParams[aidx]
	return ledgercore.AppParamsDelta{Params: &params}, ok, nil // XXX make a copy?
}

func (ml *mockLedger) lookupAssetParams(addr basics.Address, aidx basics.AssetIndex, cacheOnly bool) (ledgercore.AssetParamsDelta, bool, error) {
	params, ok := ml.balanceMap[addr].AssetParams[aidx]
	return ledgercore.AssetParamsDelta{Params: &params}, ok, nil
}

func (ml *mockLedger) lookupAppLocalState(addr basics.Address, aidx basics.AppIndex, cacheOnly bool) (ledgercore.AppLocalStateDelta, bool, error) {
	params, ok := ml.balanceMap[addr].AppLocalStates[aidx]
	return ledgercore.AppLocalStateDelta{LocalState: &params}, ok, nil
}

func (ml *mockLedger) lookupAssetHolding(addr basics.Address, aidx basics.AssetIndex, cacheOnly bool) (ledgercore.AssetHoldingDelta, bool, error) {
	params, ok := ml.balanceMap[addr].Assets[aidx]
	return ledgercore.AssetHoldingDelta{Holding: &params}, ok, nil
}

func (ml *mockLedger) checkDup(firstValid, lastValid basics.Round, txn transactions.Txid, txl ledgercore.Txlease) error {
	return nil
}

func (ml *mockLedger) getCreator(cidx basics.CreatableIndex, ctype basics.CreatableType) (basics.Address, bool, error) {
	return basics.Address{}, false, nil
}

func (ml *mockLedger) getStorageCounts(addr basics.Address, aidx basics.AppIndex, global bool) (basics.StateSchema, error) {
	return basics.StateSchema{}, nil
}

func (ml *mockLedger) getStorageLimits(addr basics.Address, aidx basics.AppIndex, global bool) (basics.StateSchema, error) {
	return basics.StateSchema{}, nil
}

func (ml *mockLedger) allocated(addr basics.Address, aidx basics.AppIndex, global bool) (bool, error) {
	return true, nil
}

func (ml *mockLedger) getKey(addr basics.Address, aidx basics.AppIndex, global bool, key string, accountIdx uint64) (basics.TealValue, bool, error) {
	return basics.TealValue{}, false, nil
}

func (ml *mockLedger) txnCounter() uint64 {
	return 0
}

func (ml *mockLedger) compactCertNext() basics.Round {
	return 0
}

func (ml *mockLedger) blockHdr(rnd basics.Round) (bookkeeping.BlockHeader, error) {
	err, hit := ml.blockErr[rnd]
	if hit {
		return bookkeeping.BlockHeader{}, err
	}
	hdr := ml.blocks[rnd] // default struct is fine if nothing found
	return hdr, nil
}

func (ml *mockLedger) blockHdrCached(rnd basics.Round) (bookkeeping.BlockHeader, error) {
	return ml.blockHdr(rnd)
}

func checkCowByUpdate(t *testing.T, cow *roundCowState, delta ledgercore.AccountDeltas) {
	for i := 0; i < delta.Len(); i++ {
		addr, data := delta.GetByIdx(i)
		d, err := cow.lookup(addr)
		require.NoError(t, err)
		require.Equal(t, d, data)
	}

	d, err := cow.lookup(ledgertesting.RandomAddress())
	require.NoError(t, err)
	require.Equal(t, d, ledgercore.AccountData{})
}

func checkCow(t *testing.T, cow *roundCowState, accts map[basics.Address]basics.AccountData) {
	for addr, data := range accts {
		d, err := cow.lookup(addr)
		require.NoError(t, err)
		require.Equal(t, d, ledgercore.ToAccountData(data))
	}

	d, err := cow.lookup(ledgertesting.RandomAddress())
	require.NoError(t, err)
	require.Equal(t, d, ledgercore.AccountData{})
}

func applyUpdates(cow *roundCowState, updates ledgercore.AccountDeltas) {
	for i := 0; i < updates.Len(); i++ {
		addr, delta := updates.GetByIdx(i)
		cow.putAccount(addr, delta)
	}
}

func TestCowBalance(t *testing.T) {
	partitiontest.PartitionTest(t)

	accts0 := ledgertesting.RandomAccounts(20, true)
	ml := mockLedger{balanceMap: accts0}

	c0 := makeRoundCowState(
		&ml, bookkeeping.BlockHeader{}, config.Consensus[protocol.ConsensusCurrentVersion],
		0, ledgercore.AccountTotals{}, 0)
	checkCow(t, c0, accts0)

	c1 := c0.child(0)
	checkCow(t, c0, accts0)
	checkCow(t, c1, accts0)

	updates1, _, _ := ledgertesting.RandomDeltas(10, accts0, 0)
	applyUpdates(c1, updates1)
	checkCow(t, c0, accts0)
	checkCowByUpdate(t, c1, updates1)

	c2 := c1.child(0)
	checkCow(t, c0, accts0)
	checkCowByUpdate(t, c1, updates1)
	checkCowByUpdate(t, c2, updates1)

	accts1 := make(map[basics.Address]basics.AccountData, updates1.Len())
	for i := 0; i < updates1.Len(); i++ {
		addr, _ := updates1.GetByIdx(i)
		var ok bool
		accts1[addr], ok = updates1.GetBasicsAccountData(addr)
		require.True(t, ok)
	}

	checkCow(t, c1, accts1)
	checkCow(t, c2, accts1)

	updates2, _, _ := ledgertesting.RandomDeltas(10, accts1, 0)
	applyUpdates(c2, updates2)
	checkCowByUpdate(t, c1, updates1)
	checkCowByUpdate(t, c2, updates2)

	c2.commitToParent()
	checkCow(t, c0, accts0)
	checkCowByUpdate(t, c1, updates2)

	c1.commitToParent()
	checkCowByUpdate(t, c0, updates2)
}
