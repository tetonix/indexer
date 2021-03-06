// Copyright (C) 2019-2020 Algorand, Inc.
// This file is part of the Algorand Indexer
//
// Algorand Indexer is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// Algorand Indexer is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with Algorand Indexer.  If not, see <https://www.gnu.org/licenses/>.

package accounting

import (
	"bytes"
	"fmt"

	//"github.com/algorand/go-algorand-sdk/encoding/json"
	"github.com/algorand/go-algorand-sdk/encoding/msgpack"

	"github.com/algorand/indexer/idb"
	"github.com/algorand/indexer/types"
)

type AccountingState struct {
	db idb.IndexerDb

	defaultFrozen map[uint64]bool

	currentRound uint64

	idb.RoundUpdates

	feeAddr    types.Address
	rewardAddr types.Address

	rewardsLevel uint64

	// number of txns at the end of the previous block
	txnCounter uint64
}

func New(db idb.IndexerDb) *AccountingState {
	return &AccountingState{db: db, defaultFrozen: make(map[uint64]bool)}
}

func foo(db idb.IndexerDb) {
	//var defaultFrozen map[uint64]bool
}

func (accounting *AccountingState) initRound(round uint64) error {
	block, err := accounting.db.GetBlock(round)
	if err != nil {
		return err
	}
	accounting.feeAddr = block.FeeSink
	accounting.rewardAddr = block.RewardsPool
	accounting.rewardsLevel = block.RewardsLevel
	accounting.currentRound = round
	return nil
}

func (accounting *AccountingState) commitRound() error {
	err := accounting.db.CommitRoundAccounting(accounting.RoundUpdates, accounting.currentRound, accounting.rewardsLevel)
	if err != nil {
		return err
	}
	accounting.AlgoUpdates = nil
	accounting.AssetUpdates = nil
	accounting.AcfgUpdates = nil
	accounting.FreezeUpdates = nil
	accounting.AssetCloses = nil
	accounting.AssetDestroys = nil
	return nil
}

func (accounting *AccountingState) Close() error {
	return accounting.commitRound()
}

var zeroAddr = [32]byte{}

func addrIsZero(a types.Address) bool {
	return bytes.Equal(a[:], zeroAddr[:])
}

func (accounting *AccountingState) updateAlgo(addr types.Address, d int64) {
	if accounting.AlgoUpdates == nil {
		accounting.AlgoUpdates = make(map[[32]byte]int64)
	}
	accounting.AlgoUpdates[addr] = accounting.AlgoUpdates[addr] + d
}

func (accounting *AccountingState) updateAsset(addr types.Address, assetId uint64, d int64) {
	accounting.AssetUpdates = append(accounting.AssetUpdates, idb.AssetUpdate{Addr: addr, AssetId: assetId, Delta: d, DefaultFrozen: accounting.defaultFrozen[assetId]})
}

func (accounting *AccountingState) closeAsset(from types.Address, assetId uint64, to types.Address) {
	accounting.AssetCloses = append(accounting.AssetCloses, idb.AssetClose{CloseTo: to, AssetId: assetId, Sender: from})
}
func (accounting *AccountingState) freezeAsset(addr types.Address, assetId uint64, frozen bool) {
	accounting.FreezeUpdates = append(accounting.FreezeUpdates, idb.FreezeUpdate{Addr: addr, AssetId: assetId, Frozen: frozen})
}
func (accounting *AccountingState) destroyAsset(assetId uint64) {
	accounting.AssetDestroys = append(accounting.AssetDestroys, assetId)
}

func (accounting *AccountingState) AddTransaction(round uint64, intra int, txnbytes []byte) (err error) {
	var stxn types.SignedTxnInBlock
	err = msgpack.Decode(txnbytes, &stxn)
	if err != nil {
		return fmt.Errorf("txn r=%d i=%d failed decode, %v\n", round, intra, err)
	}
	if accounting.currentRound != round {
		err = accounting.commitRound()
		if err != nil {
			return fmt.Errorf("add tx commit round %d, %v", accounting.currentRound, err)
		}
		err = accounting.initRound(round)
		if err != nil {
			return fmt.Errorf("add tx init round %d, %v", round, err)
		}
	}

	accounting.updateAlgo(stxn.Txn.Sender, -int64(stxn.Txn.Fee))
	accounting.updateAlgo(accounting.feeAddr, int64(stxn.Txn.Fee))

	if stxn.SenderRewards != 0 {
		accounting.updateAlgo(stxn.Txn.Sender, int64(stxn.SenderRewards))
		accounting.updateAlgo(accounting.rewardAddr, -int64(stxn.SenderRewards))
	}

	if stxn.Txn.Type == "pay" {
		amount := int64(stxn.Txn.Amount)
		if amount != 0 {
			accounting.updateAlgo(stxn.Txn.Sender, -amount)
			accounting.updateAlgo(stxn.Txn.Receiver, amount)
		}
		if stxn.ClosingAmount != 0 {
			accounting.updateAlgo(stxn.Txn.Sender, -int64(stxn.ClosingAmount))
			accounting.updateAlgo(stxn.Txn.CloseRemainderTo, int64(stxn.ClosingAmount))
		}
		if stxn.ReceiverRewards != 0 {
			accounting.updateAlgo(stxn.Txn.Receiver, int64(stxn.ReceiverRewards))
			accounting.updateAlgo(accounting.rewardAddr, -int64(stxn.ReceiverRewards))
		}
		if stxn.CloseRewards != 0 {
			accounting.updateAlgo(stxn.Txn.CloseRemainderTo, int64(stxn.CloseRewards))
			accounting.updateAlgo(accounting.rewardAddr, -int64(stxn.CloseRewards))
		}
	} else if stxn.Txn.Type == "keyreg" {
		// TODO: record keys?
	} else if stxn.Txn.Type == "acfg" {
		assetId := uint64(stxn.Txn.ConfigAsset)
		if assetId == 0 {
			assetId = accounting.txnCounter + uint64(intra) + 1
		}
		if stxn.Txn.AssetParams.IsZero() {
			accounting.destroyAsset(assetId)
		} else {
			accounting.AcfgUpdates = append(accounting.AcfgUpdates, idb.AcfgUpdate{AssetId: assetId, Creator: stxn.Txn.Sender, Params: stxn.Txn.AssetParams})
			accounting.defaultFrozen[assetId] = stxn.Txn.AssetParams.DefaultFrozen
			if stxn.Txn.ConfigAsset == 0 {
				// initial creation, give all initial value to creator
				if stxn.Txn.AssetParams.Total != 0 {
					accounting.updateAsset(stxn.Txn.Sender, assetId, int64(stxn.Txn.AssetParams.Total))
				}
			}
		}
	} else if stxn.Txn.Type == "axfer" {
		sender := stxn.Txn.AssetSender // closeout
		if sender.IsZero() {
			sender = stxn.Txn.Sender
		}
		if stxn.Txn.AssetAmount != 0 {
			accounting.updateAsset(sender, uint64(stxn.Txn.XferAsset), -int64(stxn.Txn.AssetAmount))
			accounting.updateAsset(stxn.Txn.AssetReceiver, uint64(stxn.Txn.XferAsset), int64(stxn.Txn.AssetAmount))
		}
		if !stxn.Txn.AssetCloseTo.IsZero() {
			accounting.closeAsset(sender, uint64(stxn.Txn.XferAsset), stxn.Txn.AssetCloseTo)
		}
	} else if stxn.Txn.Type == "afrz" {
		accounting.freezeAsset(stxn.Txn.FreezeAccount, uint64(stxn.Txn.FreezeAsset), stxn.Txn.AssetFrozen)
	} else {
		return fmt.Errorf("txn r=%d i=%d UNKNOWN TYPE %#v\n", round, intra, stxn.Txn.Type)
	}
	return nil
}

// TODO: copy rewards calculation logic from go-algorand so that we can present full balance including unrealized rewards.
