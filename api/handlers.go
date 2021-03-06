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

package api

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/algorand/go-algorand-sdk/client/algod/models"
	"github.com/algorand/go-algorand-sdk/crypto"
	"github.com/algorand/go-algorand-sdk/encoding/msgpack"
	atypes "github.com/algorand/go-algorand-sdk/types"
	"github.com/gorilla/mux"

	"github.com/algorand/indexer/types"
)

// parseTime same as go-algorand/daemon/algod/api/server/v1/handlers/handlers.go
func parseTime(t string) (res time.Time, err error) {
	// check for just date
	res, err = time.Parse("2006-01-02", t)
	if err == nil {
		return
	}

	// check for date and time
	res, err = time.Parse(time.RFC3339, t)
	if err == nil {
		return
	}

	return
}

func formUint64(r *http.Request, keySynonyms []string, defaultValue uint64) (value uint64, err error) {
	value = defaultValue
	for _, key := range keySynonyms {
		svalues, any := r.Form[key]
		if !any || len(svalues) < 1 {
			continue
		}
		// last value wins. or should we make repetition a 400 err?
		svalue := svalues[len(svalues)-1]
		if len(svalue) == 0 {
			continue
		}
		value, err = strconv.ParseUint(svalue, 10, 64)
		return
	}
	return
}

/*
func formUint64(r *http.Request, key string, defaultValue uint64) (value uint64, err error) {
	return formUint64Synonyms(r, []string{key}, defaultValue)
}
*/

func formTime(r *http.Request, keySynonyms []string) (value time.Time, err error) {
	for _, key := range keySynonyms {
		svalues, any := r.Form[key]
		if !any || len(svalues) < 1 {
			continue
		}
		// last value wins. or should we make repetition a 400 err?
		svalue := svalues[len(svalues)-1]
		if len(svalue) == 0 {
			continue
		}
		value, err = parseTime(svalue)
		return
	}
	return
}

type listAccountsReply struct {
	Accounts []models.Account `json:"accounts,omitempty"`
}

// ListAccounts is the http api handler that lists accounts and basic data
// /v1/accounts
// ?gt={addr} // return assets greater than some addr, for paging
// ?assets=1 // return AssetHolding for assets owned by this account
// ?assetParams=1 // return AssetParams for assets created by this account
// ?limit=N
// return {"accounts":[]models.Account}
func ListAccounts(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	var gtAddr types.Address
	accounts, err := IndexerDb.GetAccounts(r.Context(), gtAddr, 10000)
	if err != nil {
		log.Println("ListAccounts ", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	out := listAccountsReply{Accounts: accounts}
	enc := json.NewEncoder(w)

	err = enc.Encode(out)
}

// TransactionsForAddress returns transactions for some account.
// most-recent first, into the past.
// ?limit=N  default 100? 10? 50? 20 kB?
// ?firstRound=N
// ?lastRound=N
// ?afterTime=timestamp string
// ?beforeTime=timestamp string
// TODO: ?type=pay/keyreg/acfg/axfr/afrz
// Algod had ?fromDate ?toDate
// Where ???timestamp string??? is either YYYY-MM-DD or RFC3339 = "2006-01-02T15:04:05Z07:00"
//
// return {"transactions":[]models.Transaction}
// /v1/account/{address}/transactions TransactionsForAddress
func TransactionsForAddress(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	queryAddr := mux.Vars(r)["address"]
	addr, err := atypes.DecodeAddress(queryAddr)
	if err != nil {
		log.Println("bad addr, ", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	limit, err := formUint64(r, []string{"limit", "l"}, 0)
	if err != nil {
		log.Println("bad limit, ", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	firstRound, err := formUint64(r, []string{"firstRound", "fr"}, 0)
	if err != nil {
		log.Println("bad firstRound, ", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	lastRound, err := formUint64(r, []string{"lastRound", "lr"}, 0)
	if err != nil {
		log.Println("bad lastRound, ", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	beforeTime, err := formTime(r, []string{"beforeTime", "bt", "toDate"})
	if err != nil {
		log.Println("bad beforeTime, ", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	afterTime, err := formTime(r, []string{"afterTime", "at", "fromDate"})
	if err != nil {
		log.Println("bad afterTime, ", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	txns := IndexerDb.TransactionsForAddress(r.Context(), addr, limit, firstRound, lastRound, beforeTime, afterTime)

	result := transactionsListReturnObject{}
	result.Transactions = make([]models.Transaction, 0)
	for txnRow := range txns {
		var stxn types.SignedTxnInBlock
		err = msgpack.Decode(txnRow.TxnBytes, &stxn)
		if err != nil {
			log.Println("error decoding txnbytes, ", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var mtxn models.Transaction
		setApiTxn(&mtxn, stxn)
		result.Transactions = append(result.Transactions, mtxn)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	err = writeJson(&result, w)
	if err != nil {
		log.Println("transactions json out, ", err)
	}
}

func addrJson(addr atypes.Address) string {
	if addr.IsZero() {
		return ""
	}
	return addr.String()
}

func setApiTxn(out *models.Transaction, stxn types.SignedTxnInBlock) {
	out.Type = stxn.Txn.Type
	out.TxID = crypto.TransactionIDString(stxn.Txn)
	out.From = addrJson(stxn.Txn.Sender)
	out.Fee = uint64(stxn.Txn.Fee)
	out.FirstRound = uint64(stxn.Txn.FirstValid)
	out.LastRound = uint64(stxn.Txn.LastValid)
	out.Note = models.Bytes(stxn.Txn.Note)
	// out.ConfirmedRound // TODO
	// TODO: out.TransactionResults = &TransactionResults{CreatedAssetIndex: 0}
	// TODO: add Group field
	// TODO: add other txn types!
	switch stxn.Txn.Type {
	case atypes.PaymentTx:
		out.Payment = &models.PaymentTransactionType{
			To:               addrJson(stxn.Txn.Receiver),
			CloseRemainderTo: addrJson(stxn.Txn.CloseRemainderTo),
			CloseAmount:      uint64(stxn.ClosingAmount),
			Amount:           uint64(stxn.Txn.Amount),
			ToRewards:        uint64(stxn.ReceiverRewards),
		}
	case atypes.KeyRegistrationTx:
		log.Println("WARNING TODO implement keyreg")
	case atypes.AssetConfigTx:
		log.Println("WARNING TODO implement acfg")
	case atypes.AssetTransferTx:
		log.Println("WARNING TODO implement axfer")
	case atypes.AssetFreezeTx:
		log.Println("WARNING TODO implement afrz")
	}
	out.FromRewards = uint64(stxn.SenderRewards)
	out.GenesisID = stxn.Txn.GenesisID
	out.GenesisHash = stxn.Txn.GenesisHash[:]

}

type transactionsListReturnObject struct {
	Transactions []models.Transaction `json:"transactions,omitempty"`
}

func writeJson(obj interface{}, w io.Writer) error {
	enc := json.NewEncoder(w)
	return enc.Encode(obj)
}
