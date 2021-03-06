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

// You can build without postgres by `go build --tags nopostgres` but it's on by default
// +build !nopostgres

package idb

// import text to contstant setup_postgres_sql
//go:generate go run ../cmd/texttosource/main.go idb setup_postgres.sql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/algorand/go-algorand-sdk/client/algod/models"
	"github.com/algorand/go-algorand-sdk/encoding/json"
	"github.com/algorand/go-algorand-sdk/encoding/msgpack"
	atypes "github.com/algorand/go-algorand-sdk/types"
	_ "github.com/lib/pq"

	"github.com/algorand/indexer/types"
)

func OpenPostgres(connection string) (idb IndexerDb, err error) {
	db, err := sql.Open("postgres", connection)
	if err != nil {
		return nil, err
	}
	pdb := &postgresIndexerDb{db: db}
	err = pdb.init()
	idb = pdb
	return
}

type postgresIndexerDb struct {
	db *sql.DB
	tx *sql.Tx
}

func (db *postgresIndexerDb) init() (err error) {
	_, err = db.db.Exec(setup_postgres_sql)
	return
}

func (db *postgresIndexerDb) AlreadyImported(path string) (imported bool, err error) {
	row := db.db.QueryRow(`SELECT COUNT(path) FROM imported WHERE path = $1`, path)
	numpath := 0
	err = row.Scan(&numpath)
	return numpath == 1, err
}

func (db *postgresIndexerDb) MarkImported(path string) (err error) {
	_, err = db.db.Exec(`INSERT INTO imported (path) VALUES ($1)`, path)
	return err
}

func (db *postgresIndexerDb) StartBlock() (err error) {
	db.tx, err = db.db.BeginTx(context.Background(), nil)
	return
}

func (db *postgresIndexerDb) AddTransaction(round uint64, intra int, txtypeenum int, assetid uint64, txnbytes []byte, txn types.SignedTxnInBlock, participation [][]byte) error {
	// TODO: set txn_participation
	var err error
	_, err = db.tx.Exec(`INSERT INTO txn (round, intra, typeenum, asset, txnbytes, txn) VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT DO NOTHING`, round, intra, txtypeenum, assetid, txnbytes, string(json.Encode(txn)))
	if err != nil {
		return err
	}
	stmt, err := db.tx.Prepare(`INSERT INTO txn_participation (addr, round, intra) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`)
	if err != nil {
		return err
	}
	for _, paddr := range participation {
		_, err = stmt.Exec(paddr, round, intra)
		if err != nil {
			return err
		}
	}
	return err
}
func (db *postgresIndexerDb) CommitBlock(round uint64, timestamp int64, rewardslevel uint64, headerbytes []byte) error {
	var err error
	_, err = db.tx.Exec(`INSERT INTO block_header (round, realtime, rewardslevel, header) VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`, round, time.Unix(timestamp, 0), rewardslevel, headerbytes)
	if err != nil {
		return err
	}
	err = db.tx.Commit()
	db.tx = nil
	return err
}

func (db *postgresIndexerDb) GetBlockHeader(round uint64) (block types.Block, err error) {
	row := db.db.QueryRow(`SELECT header FROM block_header WHERE round = $1`, round)
	var blockbytes []byte
	err = row.Scan(&blockbytes)
	if err != nil {
		return
	}
	err = msgpack.Decode(blockbytes, &block)
	return
}

// GetAsset return AssetParams about an asset
func (db *postgresIndexerDb) GetAsset(assetid uint64) (asset types.AssetParams, err error) {
	row := db.db.QueryRow(`SELECT params FROM asset WHERE index = $1`, assetid)
	var assetjson string
	err = row.Scan(&assetjson)
	if err != nil {
		return
	}
	err = json.Decode([]byte(assetjson), &asset)
	return
}

// GetDefaultFrozen get {assetid:default frozen, ...} for all assets
func (db *postgresIndexerDb) GetDefaultFrozen() (defaultFrozen map[uint64]bool, err error) {
	rows, err := db.db.Query(`SELECT index, params -> 'df' FROM asset`)
	if err != nil {
		return
	}
	defaultFrozen = make(map[uint64]bool)
	for rows.Next() {
		var assetid uint64
		var frozen bool
		err = rows.Scan(&assetid, &frozen)
		if err != nil {
			return
		}
		defaultFrozen[assetid] = frozen
	}
	return
}

func (db *postgresIndexerDb) LoadGenesis(genesis types.Genesis) (err error) {
	tx, err := db.db.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback() // ignored if .Commit() first

	setAccount, err := tx.Prepare(`INSERT INTO account (addr, microalgos, rewardsbase, account_data) VALUES ($1, $2, 0, $3)`)
	if err != nil {
		return
	}
	defer setAccount.Close()

	total := uint64(0)
	for ai, alloc := range genesis.Allocation {
		addr, err := atypes.DecodeAddress(alloc.Address)
		if len(alloc.State.AssetParams) > 0 || len(alloc.State.Assets) > 0 {
			return fmt.Errorf("genesis account[%d] has unhandled asset", ai)
		}
		_, err = setAccount.Exec(addr[:], alloc.State.MicroAlgos, string(json.Encode(alloc.State)))
		total += uint64(alloc.State.MicroAlgos)
		if err != nil {
			return fmt.Errorf("error setting genesis account[%d], %v", ai, err)
		}
	}
	err = tx.Commit()
	fmt.Printf("genesis %d accounts %d microalgos, %v\n", len(genesis.Allocation), total, err)
	return err

}

func (db *postgresIndexerDb) GetMetastate(key string) (jsonStrValue string, err error) {
	row := db.db.QueryRow(`SELECT v FROM metastate WHERE k = $1`, key)
	err = row.Scan(&jsonStrValue)
	if err == sql.ErrNoRows {
		err = nil
	}
	return
}

func (db *postgresIndexerDb) SetMetastate(key, jsonStrValue string) (err error) {
	_, err = db.db.Exec(`INSERT INTO metastate (k, v) VALUES ($1, $2) ON CONFLICT (k) DO UPDATE SET v = EXCLUDED.v`, key, jsonStrValue)
	return
}

func (db *postgresIndexerDb) yieldTxnsThread(ctx context.Context, rows *sql.Rows, results chan<- TxnRow) {
	for rows.Next() {
		var round uint64
		var intra int
		var txnbytes []byte
		err := rows.Scan(&round, &intra, &txnbytes)
		var row TxnRow
		if err != nil {
			row.Error = err
		} else {
			row.Round = round
			row.Intra = intra
			row.TxnBytes = txnbytes
		}
		select {
		case <-ctx.Done():
			break
		case results <- row:
			if err != nil {
				break
			}
		}
	}
	close(results)
}

func (db *postgresIndexerDb) YieldTxns(ctx context.Context, prevRound int64) <-chan TxnRow {
	results := make(chan TxnRow, 1)
	rows, err := db.db.QueryContext(ctx, `SELECT round, intra, txnbytes FROM txn WHERE round > $1 ORDER BY round, intra`, prevRound)
	if err != nil {
		results <- TxnRow{Error: err}
		close(results)
		return results
	}
	go db.yieldTxnsThread(ctx, rows, results)
	return results
}

func (db *postgresIndexerDb) CommitRoundAccounting(updates RoundUpdates, round, rewardsBase uint64) (err error) {
	any := false
	tx, err := db.db.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback() // ignored if .Commit() first

	if len(updates.AlgoUpdates) > 0 {
		any = true
		// account_data json is only used on account creation, otherwise the account data jsonb field is updated from the delta
		setalgo, err := tx.Prepare(`INSERT INTO account (addr, microalgos, rewardsbase) VALUES ($1, $2, $3) ON CONFLICT (addr) DO UPDATE SET microalgos = account.microalgos + EXCLUDED.microalgos, rewardsbase = EXCLUDED.rewardsbase`)
		if err != nil {
			return fmt.Errorf("prepare update algo, %v", err)
		}
		defer setalgo.Close()
		for addr, delta := range updates.AlgoUpdates {
			_, err = setalgo.Exec(addr[:], delta, rewardsBase)
			if err != nil {
				return fmt.Errorf("update algo, %v", err)
			}
		}
	}
	if len(updates.AcfgUpdates) > 0 {
		any = true
		setacfg, err := tx.Prepare(`INSERT INTO asset (index, creator_addr, params) VALUES ($1, $2, $3) ON CONFLICT (index) DO UPDATE SET params = EXCLUDED.params`)
		if err != nil {
			return fmt.Errorf("prepare set asset, %v", err)
		}
		defer setacfg.Close()
		for _, au := range updates.AcfgUpdates {
			_, err = setacfg.Exec(au.AssetId, au.Creator[:], string(json.Encode(au.Params)))
			if err != nil {
				return fmt.Errorf("update asset, %v", err)
			}
		}
	}
	if len(updates.AssetUpdates) > 0 {
		any = true
		seta, err := tx.Prepare(`INSERT INTO account_asset (addr, assetid, amount, frozen) VALUES ($1, $2, $3, $4) ON CONFLICT (addr, assetid) DO UPDATE SET amount = account_asset.amount + EXCLUDED.amount`)
		if err != nil {
			return fmt.Errorf("prepare set account_asset, %v", err)
		}
		defer seta.Close()
		for _, au := range updates.AssetUpdates {
			_, err = seta.Exec(au.Addr[:], au.AssetId, au.Delta, au.DefaultFrozen)
			if err != nil {
				return fmt.Errorf("update account asset, %v", err)
			}
		}
	}
	if len(updates.FreezeUpdates) > 0 {
		any = true
		fr, err := tx.Prepare(`INSERT INTO account_asset (addr, assetid, amount, frozen) VALUES ($1, $2, 0, $3) ON CONFLICT (addr, assetid) DO UPDATE SET frozen = EXCLUDED.frozen`)
		if err != nil {
			return fmt.Errorf("prepare asset freeze, %v", err)
		}
		defer fr.Close()
		for _, fs := range updates.FreezeUpdates {
			_, err = fr.Exec(fs.Addr[:], fs.AssetId, fs.Frozen)
			if err != nil {
				return fmt.Errorf("update asset freeze, %v", err)
			}
		}
	}
	if len(updates.AssetCloses) > 0 {
		any = true
		acs, err := tx.Prepare(`INSERT INTO account_asset (addr, assetid, amount)
SELECT $1, $2, x.amount FROM account_asset x WHERE x.addr = $3
ON CONFLICT (addr, assetid) DO UPDATE SET amount = account_asset.amount + EXCLUDED.amount`)
		if err != nil {
			return fmt.Errorf("prepare asset close1, %v", err)
		}
		defer acs.Close()
		acd, err := tx.Prepare(`DELETE FROM account_asset WHERE addr = $1`)
		if err != nil {
			return fmt.Errorf("prepare asset close2, %v", err)
		}
		defer acd.Close()
		for _, ac := range updates.AssetCloses {
			_, err = acs.Exec(ac.CloseTo[:], ac.AssetId, ac.Sender[:])
			if err != nil {
				return fmt.Errorf("asset close send, %v", err)
			}
			_, err = acd.Exec(ac.Sender[:])
			if err != nil {
				return fmt.Errorf("asset close del, %v", err)
			}
		}
	}
	if len(updates.AssetDestroys) > 0 {
		any = true
		// Note! leaves `asset` row present for historical reference, but deletes all holdings from all accounts
		ads, err := tx.Prepare(`DELETE FROM account_asset WHERE assetid = $1`)
		if err != nil {
			return fmt.Errorf("prepare asset destroy, %v", err)
		}
		defer ads.Close()
		for _, assetId := range updates.AssetDestroys {
			ads.Exec(assetId)
			if err != nil {
				return fmt.Errorf("asset destroy, %v", err)
			}
		}
	}
	if !any {
		return
	}
	var istate ImportState
	staterow := tx.QueryRow(`SELECT v FROM metastate WHERE k = 'state'`)
	var stateJsonStr string
	err = staterow.Scan(&stateJsonStr)
	if err == sql.ErrNoRows {
		// ok
	} else if err != nil {
		return
	} else {
		err = json.Decode([]byte(stateJsonStr), &istate)
		if err != nil {
			return
		}
	}
	istate.AccountRound = int64(round)
	sjs := string(json.Encode(istate))
	_, err = tx.Exec(`INSERT INTO metastate (k, v) VALUES ('state', $1) ON CONFLICT (k) DO UPDATE SET v = EXCLUDED.v`, sjs)
	if err != nil {
		return
	}
	return tx.Commit()
}

func (db *postgresIndexerDb) GetBlock(round uint64) (block types.Block, err error) {
	row := db.db.QueryRow(`SELECT header FROM block_header WHERE round = $1`, round)
	var blockheaderbytes []byte
	err = row.Scan(&blockheaderbytes)
	if err != nil {
		return
	}
	err = msgpack.Decode(blockheaderbytes, &block)
	return
}

func (db *postgresIndexerDb) TransactionsForAddress(ctx context.Context, addr types.Address, limit, firstRound, lastRound uint64, beforeTime, afterTime time.Time) <-chan TxnRow {
	const maxWhereParts = 6
	whereParts := make([]string, 0, maxWhereParts)
	whereArgs := make([]interface{}, 0, maxWhereParts)
	whereParts = append(whereParts, "p.addr = $1")
	whereArgs = append(whereArgs, addr[:])
	partNumber := 2
	joinHeader := false
	if firstRound != 0 {
		whereParts = append(whereParts, fmt.Sprintf("t.round >= $%d", partNumber))
		whereArgs = append(whereArgs, firstRound)
		partNumber++
	}
	if lastRound != 0 {
		whereParts = append(whereParts, fmt.Sprintf("t.round <= $%d", partNumber))
		whereArgs = append(whereArgs, lastRound)
		partNumber++
	}
	if !beforeTime.IsZero() {
		whereParts = append(whereParts, fmt.Sprintf("h.realtime < $%d", partNumber))
		whereArgs = append(whereArgs, beforeTime)
		partNumber++
		joinHeader = true
	}
	if !afterTime.IsZero() {
		whereParts = append(whereParts, fmt.Sprintf("h.realtime > $%d", partNumber))
		whereArgs = append(whereArgs, afterTime)
		partNumber++
		joinHeader = true
	}
	var query string
	// TODO LIMIT
	whereStr := strings.Join(whereParts, " AND ")
	if joinHeader {
		query = "SELECT t.round, t.intra, t.txnbytes FROM txn t JOIN txn_participation p ON t.round = p.round AND t.intra = p.intra JOIN block_header h ON t.round = h.round WHERE " + whereStr
	} else {
		query = "SELECT t.round, t.intra, t.txnbytes FROM txn t JOIN txn_participation p ON t.round = p.round AND t.intra = p.intra WHERE " + whereStr
	}
	out := make(chan TxnRow, 1)
	rows, err := db.db.QueryContext(ctx, query, whereArgs...)
	if err != nil {
		out <- TxnRow{Error: err}
		close(out)
		return out
	}
	go db.yieldTxnsThread(ctx, rows, out)
	return out
}

const maxAccountsLimit = 1000

func (db *postgresIndexerDb) GetAccounts(ctx context.Context, greaterThan types.Address, limit int) (accounts []models.Account, err error) {
	if limit == 0 || limit > maxAccountsLimit {
		limit = maxAccountsLimit
	}
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer tx.Rollback()
	roundrow := tx.QueryRow(`SELECT (v -> 'account_round')::bigint FROM metastate WHERE k = 'state'`)
	var round uint64
	err = roundrow.Scan(&round)
	if err != nil {
		return
	}
	rows, err := tx.QueryContext(ctx, `SELECT addr, microalgos, rewardsbase, account_data FROM account WHERE addr > $1 ORDER BY addr LIMIT $2`, greaterThan[:], limit)
	if err != nil {
		return
	}
	out := make([]models.Account, 0, limit)
	for rows.Next() {
		var addr []byte
		var microalgos uint64
		var rewardsbase uint64
		var dataJsonStr *string
		err = rows.Scan(&addr, &microalgos, &rewardsbase, &dataJsonStr)
		if err != nil {
			return
		}
		var account models.Account
		account.Round = round
		var aaddr atypes.Address
		if len(addr) != 32 {
			return nil, errors.New("loaded invalid addr from db in GetAccounts")
		}
		copy(aaddr[:], addr)
		account.Address = aaddr.String()
		account.AmountWithoutPendingRewards = microalgos
		// todo, get rewardslevel at round, calculate PendingRewards and add to get Amount
		// account.Rewards // not filled
		// account.Status // not filled
		// account.AssetParams // TODO: optionally join with asset created by this addr
		// account.Assets // TODO: optionally join with account_asset
		out = append(out, account)
	}

	return out, nil
}

type postgresFactory struct {
}

func (df postgresFactory) Name() string {
	return "postgres"
}
func (df postgresFactory) Build(arg string) (IndexerDb, error) {
	return OpenPostgres(arg)
}

func init() {
	indexerFactories = append(indexerFactories, &postgresFactory{})
}

type ImportState struct {
	AccountRound int64 `codec:"account_round"`
}

func ParseImportState(js string) (istate ImportState, err error) {
	err = json.Decode([]byte(js), &istate)
	return
}
