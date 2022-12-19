package jbob

import (
	"encoding/binary"
	"github.com/multiformats/go-multihash"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/errors"
	"github.com/syndtr/goleveldb/leveldb/filter"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/storage"
	"golang.org/x/xerrors"
)

type LevelDBIndex struct {
	*leveldb.DB
}

func OpenLevelDBIndex(path string, create bool) (*LevelDBIndex, error) {
	o := &opt.Options{
		OpenFilesCacheCapacity: 500,
		ErrorIfExist:           create,
		Compression:            opt.NoCompression, // this data is quite dense
		Filter:                 filter.NewBloomFilter(10),
		// todo NoSync
	}

	var err error
	var db *leveldb.DB
	if path == "" {
		db, err = leveldb.Open(storage.NewMemStorage(), o)
	} else {
		db, err = leveldb.OpenFile(path, o)
		if errors.IsCorrupted(err) && !o.GetReadOnly() {
			db, err = leveldb.RecoverFile(path, o)
		}
	}
	if err != nil {
		return nil, xerrors.Errorf("open leveldb: %w", err)
	}

	return &LevelDBIndex{
		db,
	}, nil
}

func (l *LevelDBIndex) Has(c []multihash.Multihash) ([]bool, error) {
	out := make([]bool, len(c))
	var err error

	for i, m := range c {
		out[i], err = l.DB.Has(m, nil)
		if err != nil {
			return nil, err
		}
	}

	return out, nil
}

func (l *LevelDBIndex) Put(c []multihash.Multihash, offs []int64) error {
	batch := new(leveldb.Batch)
	for i, m := range c {
		if offs[i] == -1 {
			continue
		}
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], uint64(offs[i]))
		batch.Put(m, buf[:])
	}
	return l.DB.Write(batch, nil)
}

// Get returns offsets to data, -1 if not found
func (l *LevelDBIndex) Get(c []multihash.Multihash) ([]int64, error) {
	out := make([]int64, len(c))
	var err error

	for i, m := range c {
		v, err := l.DB.Get(m, nil)
		switch err {
		case nil:
		case leveldb.ErrNotFound:
			out[i] = -1
			continue
		default:
			return nil, xerrors.Errorf("index get: %w", err)
		}

		if len(v) != 8 {
			return nil, xerrors.Errorf("invalid value length")
		}
		out[i] = int64(binary.LittleEndian.Uint64(v))
	}

	return out, err
}

// todo sync

func (l *LevelDBIndex) Close() error {
	return l.DB.Close()
}

var _ WritableIndex = &LevelDBIndex{}
