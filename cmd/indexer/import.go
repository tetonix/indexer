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

package main

import (
	"archive/tar"
	"compress/bzip2"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	//"github.com/spf13/cobra/doc"
	//"github.com/algorand/go-algorand-sdk/encoding/msgpack"
	"github.com/algorand/go-algorand-sdk/encoding/json"

	"github.com/algorand/indexer/accounting"
	"github.com/algorand/indexer/idb"
	"github.com/algorand/indexer/importer"
	"github.com/algorand/indexer/types"
)

func maybeFail(err error, errfmt string, params ...interface{}) {
	if err == nil {
		return
	}
	fmt.Fprintf(os.Stderr, errfmt, params...)
	os.Exit(1)
}

func importTar(imp importer.Importer, tarfile io.Reader) (err error) {
	tf := tar.NewReader(tarfile)
	var header *tar.Header
	header, err = tf.Next()
	for err == nil {
		if header.Typeflag != tar.TypeReg {
			return fmt.Errorf("cannot deal with non-regular-file tar entry %#v", header.Name)
		}
		/*
			round, err := strconv.Atoi(header.Name)
			if err != nil {
				return fmt.Errorf("could not parse round number in tar archive, file %#v", header.Name)
			}
		*/
		blockbytes := make([]byte, header.Size)
		_, err = io.ReadFull(tf, blockbytes)
		if err != nil {
			return fmt.Errorf("error reading tar entry %#v: %v", header.Name, err)
		}
		err = imp.ImportBlock(blockbytes)
		if err != nil {
			return fmt.Errorf("error importing tar entry %#v: %v", header.Name, err)
		}
		header, err = tf.Next()
	}
	if err == io.EOF {
		err = nil
	}
	return
}

func importFile(db idb.IndexerDb, imp importer.Importer, fname string) {
	imported, err := db.AlreadyImported(fname)
	maybeFail(err, "%s: %v\n", fname, err)
	if imported {
		return
	}
	fmt.Printf("importing %s ...\n", fname)
	if strings.HasSuffix(fname, ".tar") {
		fin, err := os.Open(fname)
		maybeFail(err, "%s: %v\n", fname, err)
		defer fin.Close()
		err = importTar(imp, fin)
		maybeFail(err, "%s: %v\n", fname, err)
	} else if strings.HasSuffix(fname, ".tar.bz2") {
		fin, err := os.Open(fname)
		maybeFail(err, "%s: %v\n", fname, err)
		defer fin.Close()
		bzin := bzip2.NewReader(fin)
		err = importTar(imp, bzin)
		maybeFail(err, "%s: %v\n", fname, err)
	} else {
		// assume a standalone block msgpack blob
		blockbytes, err := ioutil.ReadFile(fname)
		maybeFail(err, "%s: could not read, %v\n", fname, err)
		err = imp.ImportBlock(blockbytes)
		maybeFail(err, "%s: could not import, %v\n", fname, err)
	}
	err = db.MarkImported(fname)
	maybeFail(err, "%s: %v\n", fname, err)
}

func loadGenesis(db idb.IndexerDb, in io.Reader) (err error) {
	var genesis types.Genesis
	gbytes, err := ioutil.ReadAll(in)
	if err != nil {
		return fmt.Errorf("error reading genesis, %v", err)
	}
	err = json.Decode(gbytes, &genesis)
	if err != nil {
		return fmt.Errorf("error decoding genesis, %v", err)
	}

	return db.LoadGenesis(genesis)
}

/*
type ImportState struct {
	// AccountRound is the last round committed into account state.
	// -1 for after genesis is committed and we need to load round 0
	AccountRound int64
}*/

func updateAccounting(db idb.IndexerDb) {
	stateJsonStr, err := db.GetMetastate("state")
	maybeFail(err, "getting import state, %v\n", err)
	var state idb.ImportState
	if stateJsonStr == "" {
		if genesisJsonPath != "" {
			fmt.Printf("loading genesis %s\n", genesisJsonPath)
			// if we're given no previous state and we're given a genesis file, import it as initial account state
			gf, err := os.Open(genesisJsonPath)
			maybeFail(err, "%s: %v\n", genesisJsonPath, err)
			err = loadGenesis(db, gf)
			maybeFail(err, "%s: could not load genesis json, %v\n", genesisJsonPath, err)
			state.AccountRound = -1
		} else {
			fmt.Fprintf(os.Stderr, "no import state recorded; need --genesis genesis.json file to get started\n")
			os.Exit(1)
			return
		}
	} else {
		state, err = idb.ParseImportState(stateJsonStr)
		maybeFail(err, "parsing import state, %v\n", err)
		fmt.Printf("will start from round >%d\n", state.AccountRound)
	}

	lastlog := time.Now()
	act := accounting.New(db)
	txns := db.YieldTxns(context.Background(), state.AccountRound)
	currentRound := uint64(0)
	roundsSeen := 0
	for txn := range txns {
		if txn.Round != currentRound {
			prevRound := currentRound
			roundsSeen++
			currentRound = txn.Round
			if (numRoundsLimit != 0) && (roundsSeen > numRoundsLimit) {
				break
			}
			now := time.Now()
			dt := now.Sub(lastlog)
			if dt > (5 * time.Second) {
				fmt.Printf("accounting through %d\n", prevRound)
				lastlog = now
			}
		}
		err = act.AddTransaction(txn.Round, txn.Intra, txn.TxnBytes)
		maybeFail(err, "txn accounting r=%d i=%d, %v\n", txn.Round, txn.Intra, err)
	}
	err = act.Close()
	maybeFail(err, "accounting close %v\n", err)
	fmt.Printf("accounting updated through round %d\n", currentRound)
}

var (
	genesisJsonPath string
	numRoundsLimit  int
	blockFileLimit  int
)

type blockTarPaths []string

// sort.Interface
func (paths *blockTarPaths) Len() int {
	return len(*paths)
}

func pathNameStartInt(x string) int64 {
	underscorePos := strings.IndexRune(x, '_')
	if underscorePos == -1 {
		return -1
	}
	v, err := strconv.ParseInt(x[:underscorePos], 10, 64)
	if err != nil {
		return -1
	}
	return v
}

// sort.Interface
func (paths *blockTarPaths) Less(i, j int) bool {
	return pathNameStartInt((*paths)[i]) < pathNameStartInt((*paths)[j])
}

// sort.Interface
func (paths *blockTarPaths) Swap(i, j int) {
	t := (*paths)[i]
	(*paths)[i] = (*paths)[j]
	(*paths)[j] = t
}

var importCmd = &cobra.Command{
	Use:   "import",
	Short: "import block file or tar file of blocks",
	Long:  "import block file or tar file of blocks. arguments are interpret as file globs (e.g. *.tar.bz2)",
	//Args:
	Run: func(cmd *cobra.Command, args []string) {
		// TODO: connect to db and instantiate Importer
		//imp := importer.NewPrintImporter()
		db := globalIndexerDb()
		imp := importer.NewDBImporter(db)
		for _, fname := range args {
			matches, err := filepath.Glob(fname)
			if err == nil {
				pathsSorted := blockTarPaths(matches)
				sort.Sort(&pathsSorted)
				if blockFileLimit != 0 && len(pathsSorted) > blockFileLimit {
					pathsSorted = pathsSorted[:blockFileLimit]
				}
				for _, gfname := range pathsSorted {
					//fmt.Printf("%s ...\n", gfname)
					importFile(db, imp, gfname)
				}
			} else {
				// try without passing throug glob
				importFile(db, imp, fname)
			}
		}

		updateAccounting(db)
	},
}

func init() {
	importCmd.Flags().StringVarP(&genesisJsonPath, "genesis", "g", "", "path to genesis.json")
	importCmd.Flags().IntVarP(&numRoundsLimit, "num-rounds-limit", "", 0, "number of rounds to process")
	importCmd.Flags().IntVarP(&blockFileLimit, "block-file-limit", "", 0, "number of block files to process (for debugging)")
}
