package stagedsync

import (
	"fmt"
	"os"

	"github.com/ledgerwatch/turbo-geth/common"
	"github.com/ledgerwatch/turbo-geth/common/changeset"
	"github.com/ledgerwatch/turbo-geth/common/dbutils"
	"github.com/ledgerwatch/turbo-geth/common/etl"
	"github.com/ledgerwatch/turbo-geth/core"
	"github.com/ledgerwatch/turbo-geth/core/rawdb"
	"github.com/ledgerwatch/turbo-geth/ethdb"
	"github.com/ledgerwatch/turbo-geth/log"
	"github.com/ledgerwatch/turbo-geth/trie"

	"github.com/pkg/errors"
	"github.com/ugorji/go/codec"
)

var cbor codec.CborHandle

func SpawnHashStateStage(s *StageState, stateDB ethdb.Database, datadir string, quit chan struct{}) error {
	hashProgress := s.BlockNumber

	syncHeadNumber, err := s.ExecutionAt(stateDB)
	if err != nil {
		return err
	}

	if hashProgress == syncHeadNumber {
		// we already did hash check for this block
		// we don't do the obvious `if hashProgress > syncHeadNumber` to support reorgs more naturally
		s.Done()
		return nil
	}

	if core.UsePlainStateExecution {
		log.Info("Promoting plain state", "from", hashProgress, "to", syncHeadNumber)
		err := promoteHashedState(s, stateDB, hashProgress, syncHeadNumber, datadir, quit)
		if err != nil {
			return err
		}
	}
	if err := verifyRootHash(stateDB, syncHeadNumber); err != nil {
		return err
	}
	return s.DoneAndUpdate(stateDB, syncHeadNumber)
}

func verifyRootHash(stateDB ethdb.Database, syncHeadNumber uint64) error {
	hash := rawdb.ReadCanonicalHash(stateDB, syncHeadNumber)
	syncHeadHeader := rawdb.ReadHeader(stateDB, hash, syncHeadNumber)
	log.Info("Validating root hash", "block", syncHeadNumber, "blockRoot", syncHeadHeader.Root.Hex())
	loader := trie.NewSubTrieLoader(syncHeadNumber)
	rl := trie.NewRetainList(0)
	subTries, err1 := loader.LoadFromFlatDB(stateDB, rl, nil /*HashCollector*/, [][]byte{nil}, []int{0}, false)
	if err1 != nil {
		return errors.Wrap(err1, "checking root hash failed")
	}
	if len(subTries.Hashes) != 1 {
		return fmt.Errorf("expected 1 hash, got %d", len(subTries.Hashes))
	}
	if subTries.Hashes[0] != syncHeadHeader.Root {
		return fmt.Errorf("wrong trie root: %x, expected (from header): %x", subTries.Hashes[0], syncHeadHeader.Root)
	}
	return nil
}

func unwindHashStateStage(u *UnwindState, s *StageState, stateDB ethdb.Database, datadir string, quit chan struct{}) error {
	if err := unwindHashStateStageImpl(u, s, stateDB, datadir, quit); err != nil {
		return err
	}
	if err := verifyRootHash(stateDB, u.UnwindPoint); err != nil {
		return err
	}
	if err := u.Done(stateDB); err != nil {
		return fmt.Errorf("unwind HashState: reset: %v", err)
	}
	return nil
}

func unwindHashStateStageImpl(u *UnwindState, s *StageState, stateDB ethdb.Database, datadir string, quit chan struct{}) error {
	// Currently it does not require unwinding because it does not create any Intemediate Hash records
	// and recomputes the state root from scratch
	prom := NewPromoter(stateDB, quit)
	prom.TempDir = datadir
	if err := prom.Unwind(s.BlockNumber, u.UnwindPoint, dbutils.PlainAccountChangeSetBucket); err != nil {
		return err
	}
	if err := prom.Unwind(s.BlockNumber, u.UnwindPoint, dbutils.PlainStorageChangeSetBucket); err != nil {
		return err
	}
	return nil
}

func promoteHashedState(s *StageState, db ethdb.Database, from, to uint64, datadir string, quit chan struct{}) error {
	if from == 0 {
		return promoteHashedStateCleanly(s, db, to, datadir, quit)
	}
	return promoteHashedStateIncrementally(from, to, db, datadir, quit)
}

func promoteHashedStateCleanly(s *StageState, db ethdb.Database, to uint64, datadir string, quit chan struct{}) error {
	var err error
	if err = common.Stopped(quit); err != nil {
		return err
	}
	var loadStartKey []byte
	skipCurrentState := false
	if len(s.StageData) > 0 && s.StageData[0] == byte(0xFF) {
		if len(s.StageData) == 1 {
			skipCurrentState = true
		} else {
			loadStartKey, err = etl.NextKey(s.StageData)
			return err
		}
	}

	if !skipCurrentState {
		toStateStageData := func(k []byte) []byte {
			return append([]byte{0xFF}, k...)
		}

		err = etl.Transform(
			db,
			dbutils.PlainStateBucket,
			dbutils.CurrentStateBucket,
			datadir,
			keyTransformExtractFunc(transformPlainStateKey),
			etl.IdentityLoadFunc,
			etl.TransformArgs{
				Quit:         quit,
				LoadStartKey: loadStartKey,
				OnLoadCommit: func(batch ethdb.Putter, key []byte, isDone bool) error {
					if isDone {
						return s.UpdateWithStageData(batch, s.BlockNumber, toStateStageData(nil))
					}
					return s.UpdateWithStageData(batch, s.BlockNumber, toStateStageData(key))
				},
			},
		)

		if err != nil {
			return err
		}
	}

	toCodeStageData := func(k []byte) []byte {
		return append([]byte{0xCD}, k...)
	}

	if len(s.StageData) > 0 && s.StageData[0] == byte(0xCD) {
		loadStartKey, err = etl.NextKey(s.StageData)
		return err
	}

	return etl.Transform(
		db,
		dbutils.PlainContractCodeBucket,
		dbutils.ContractCodeBucket,
		datadir,
		keyTransformExtractFunc(transformContractCodeKey),
		etl.IdentityLoadFunc,
		etl.TransformArgs{
			Quit:         quit,
			LoadStartKey: loadStartKey,
			OnLoadCommit: func(batch ethdb.Putter, key []byte, isDone bool) error {
				if isDone {
					return s.UpdateWithStageData(batch, to, nil)
				}
				return s.UpdateWithStageData(batch, s.BlockNumber, toCodeStageData(key))
			},
		},
	)
}

func keyTransformExtractFunc(transformKey func([]byte) ([]byte, error)) etl.ExtractFunc {
	return func(k, v []byte, next etl.ExtractNextFunc) error {
		newK, err := transformKey(k)
		if err != nil {
			return err
		}
		return next(k, newK, v)
	}
}

func transformPlainStateKey(key []byte) ([]byte, error) {
	switch len(key) {
	case common.AddressLength:
		// account
		hash, err := common.HashData(key)
		return hash[:], err
	case common.AddressLength + common.IncarnationLength + common.HashLength:
		// storage
		address, incarnation, key := dbutils.PlainParseCompositeStorageKey(key)
		addrHash, err := common.HashData(address[:])
		if err != nil {
			return nil, err
		}
		secKey, err := common.HashData(key[:])
		if err != nil {
			return nil, err
		}
		compositeKey := dbutils.GenerateCompositeStorageKey(addrHash, incarnation, secKey)
		return compositeKey, nil
	default:
		// no other keys are supported
		return nil, fmt.Errorf("could not convert key from plain to hashed, unexpected len: %d", len(key))
	}
}

func transformContractCodeKey(key []byte) ([]byte, error) {
	if len(key) != common.AddressLength+common.IncarnationLength {
		return nil, fmt.Errorf("could not convert code key from plain to hashed, unexpected len: %d", len(key))
	}
	address, incarnation := dbutils.PlainParseStoragePrefix(key)

	addrHash, err := common.HashData(address[:])
	if err != nil {
		return nil, err
	}

	compositeKey := dbutils.GenerateStoragePrefix(addrHash[:], incarnation)
	return compositeKey, nil
}

func keyTransformLoadFunc(k []byte, value []byte, state etl.State, next etl.LoadNextFunc) error {
	newK, err := transformPlainStateKey(k)
	if err != nil {
		return err
	}
	return next(newK, value)
}

func NewPromoter(db ethdb.Database, quitCh chan struct{}) *Promoter {
	return &Promoter{
		db:               db,
		ChangeSetBufSize: 256 * 1024 * 1024,
		TempDir:          os.TempDir(),
	}
}

type Promoter struct {
	db               ethdb.Database
	ChangeSetBufSize uint64
	TempDir          string
	quitCh           chan struct{}
}

var promoterMapper = map[string]struct {
	WalkerAdapter func(v []byte) changeset.Walker
	KeySize       int
	Template      string
}{
	string(dbutils.PlainAccountChangeSetBucket): {
		WalkerAdapter: func(v []byte) changeset.Walker {
			return changeset.AccountChangeSetPlainBytes(v)
		},
		KeySize:  common.AddressLength,
		Template: "acc-prom-",
	},
	string(dbutils.PlainStorageChangeSetBucket): {
		WalkerAdapter: func(v []byte) changeset.Walker {
			return changeset.StorageChangeSetPlainBytes(v)
		},
		KeySize:  common.AddressLength + common.IncarnationLength + common.HashLength,
		Template: "st-prom-",
	},
	string(dbutils.AccountChangeSetBucket): {
		WalkerAdapter: func(v []byte) changeset.Walker {
			return changeset.AccountChangeSetBytes(v)
		},
		KeySize:  common.HashLength,
		Template: "acc-prom-",
	},
	string(dbutils.StorageChangeSetBucket): {
		WalkerAdapter: func(v []byte) changeset.Walker {
			return changeset.StorageChangeSetBytes(v)
		},
		KeySize:  common.HashLength + common.IncarnationLength + common.HashLength,
		Template: "st-prom-",
	},
}

func getExtractFunc(changeSetBucket []byte) etl.ExtractFunc {
	mapping, ok := promoterMapper[string(changeSetBucket)]
	return func(_, changesetBytes []byte, next etl.ExtractNextFunc) error {
		if !ok {
			return fmt.Errorf("unknown bucket type: %s", changeSetBucket)
		}
		return mapping.WalkerAdapter(changesetBytes).Walk(func(k, v []byte) error {
			return next(k, k, nil)
		})
	}
}

func getUnwindExtractFunc(changeSetBucket []byte) etl.ExtractFunc {
	mapping, ok := promoterMapper[string(changeSetBucket)]
	return func(_, changesetBytes []byte, next etl.ExtractNextFunc) error {
		if !ok {
			return fmt.Errorf("unknown bucket type: %s", changeSetBucket)
		}
		return mapping.WalkerAdapter(changesetBytes).Walk(func(k, v []byte) error {
			return next(k, k, v)
		})
	}
}

func getFromPlainStateAndLoad(db ethdb.Getter, loadFunc etl.LoadFunc) etl.LoadFunc {
	return func(k []byte, value []byte, state etl.State, next etl.LoadNextFunc) error {
		value, err := db.Get(dbutils.PlainStateBucket, k)
		if err == nil || err == ethdb.ErrKeyNotFound {
			return loadFunc(k, value, state, next)
		}
		return err
	}
}

func (p *Promoter) Promote(from, to uint64, changeSetBucket []byte) error {
	log.Info("Incremental promotion started", "from", from, "to", to, "csbucket", string(changeSetBucket))

	startkey := dbutils.EncodeTimestamp(from + 1)

	return etl.Transform(
		p.db,
		changeSetBucket,
		dbutils.CurrentStateBucket,
		p.TempDir,
		getExtractFunc(changeSetBucket),
		// here we avoid getting the state from changesets,
		// we just care about the accounts that did change,
		// so we can directly read from the PlainTextBuffer
		getFromPlainStateAndLoad(p.db, keyTransformLoadFunc),
		etl.TransformArgs{
			BufferType:      etl.SortableAppendBuffer,
			ExtractStartKey: startkey,
			Quit:            p.quitCh,
		},
	)
}

func (p *Promoter) Unwind(from, to uint64, changeSetBucket []byte) error {
	log.Info("Unwinding started", "from", from, "to", to, "csbucket", string(changeSetBucket))

	startkey := dbutils.EncodeTimestamp(to + 1)

	return etl.Transform(
		p.db,
		changeSetBucket,
		dbutils.CurrentStateBucket,
		p.TempDir,
		getUnwindExtractFunc(changeSetBucket),
		keyTransformLoadFunc,
		etl.TransformArgs{
			BufferType:      etl.SortableOldestAppearedBuffer,
			ExtractStartKey: startkey,
			Quit:            p.quitCh,
		},
	)
}

func promoteHashedStateIncrementally(from, to uint64, db ethdb.Database, datadir string, quit chan struct{}) error {
	prom := NewPromoter(db, quit)
	prom.TempDir = datadir
	if err := prom.Promote(from, to, dbutils.PlainAccountChangeSetBucket); err != nil {
		return err
	}
	if err := prom.Promote(from, to, dbutils.PlainStorageChangeSetBucket); err != nil {
		return err
	}
	return nil
}
