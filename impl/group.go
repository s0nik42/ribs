package impl

import (
	"context"
	"fmt"
	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	cbor "github.com/ipfs/go-ipld-cbor"
	carutil "github.com/ipld/go-car/util"
	iface "github.com/lotus-web3/ribs"
	"github.com/lotus-web3/ribs/jbob"
	mh "github.com/multiformats/go-multihash"
	"path/filepath"
	"strconv"
	"sync"

	"golang.org/x/xerrors"
	"os"
)

var (
	// 100MB for now
	// TODO: make this configurable
	maxGroupSize int64 = 6000 << 20

	// todo enforce this
	maxGroupBlocks int64 = 20 << 20
)

type Group struct {
	db    *ribsDB
	index iface.Index

	path string
	id   int64

	state iface.GroupState

	// db lock
	// note: can be taken when jblk is held
	dblk sync.Mutex

	// jbob (with jblk)

	jblk sync.RWMutex

	inflightBlocks int64
	inflightSize   int64

	committedBlocks int64
	committedSize   int64

	jb *jbob.JBOB
}

func OpenGroup(db *ribsDB, index iface.Index, id, committedBlocks, committedSize int64, path string, state iface.GroupState, create bool) (*Group, error) {
	groupPath := filepath.Join(path, "grp", strconv.FormatInt(id, 32))

	if err := os.MkdirAll(groupPath, 0755); err != nil {
		return nil, xerrors.Errorf("create group directory: %w", err)
	}

	// open jbob

	jbOpenFunc := jbob.Open
	if create {
		jbOpenFunc = jbob.Create
	}

	jb, err := jbOpenFunc(filepath.Join(groupPath, "blk.jbmeta"), filepath.Join(groupPath, "blk.jblog"))
	if err != nil {
		return nil, xerrors.Errorf("open jbob: %w", err)
	}

	return &Group{
		db:    db,
		index: index,

		jb: jb,

		committedBlocks: committedBlocks,
		committedSize:   committedSize,

		path:  groupPath,
		id:    id,
		state: state,
	}, nil
}

func (m *Group) Put(ctx context.Context, b []blocks.Block) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}

	m.jblk.Lock()
	defer m.jblk.Unlock()

	// reserve space
	if m.state != iface.GroupStateWritable {
		return 0, nil
	}

	availSpace := maxGroupSize - m.committedSize // todo async - inflight

	var writeSize int64
	var writeBlocks int

	for _, blk := range b {
		if int64(len(blk.RawData()))+writeSize > availSpace {
			break
		}
		writeSize += int64(len(blk.RawData()))
		writeBlocks++
	}

	if writeBlocks < len(b) {
		// this group is full
		m.state = iface.GroupStateFull
	}

	m.inflightBlocks += int64(writeBlocks)
	m.inflightSize += writeSize

	// backend write

	// 1. (buffer) writes to jbob

	c := make([]mh.Multihash, len(b))
	for i, blk := range b {
		c[i] = blk.Cid().Hash()
	}

	err := m.jb.Put(c[:writeBlocks], b[:writeBlocks])
	if err != nil {
		// todo handle properly (abort, close, check disk space / resources, repopen)
		return 0, xerrors.Errorf("writing to jbob: %w", err)
	}

	// <todo async commit>

	// 2. commit jbob (so puts above are now on disk)

	at, err := m.jb.Commit()
	if err != nil {
		// todo handle properly (abort, close, check disk space / resources, repopen)
		return 0, xerrors.Errorf("committing jbob: %w", err)
	}

	m.inflightBlocks -= int64(writeBlocks)
	m.inflightSize -= writeSize
	m.committedBlocks += int64(writeBlocks)
	m.committedSize += writeSize

	// 3. write top-level index (before we update group head so replay is possible)
	err = m.index.AddGroup(ctx, c[:writeBlocks], m.id)
	if err != nil {
		// todo handle properly (abort, close, check disk space / resources, repopen)
		return 0, xerrors.Errorf("writing index: %w", err)
	}

	// 3.5 mark as read-only if full
	// todo is this the right place to do this?
	if m.state == iface.GroupStateFull {
		if err := m.jb.MarkReadOnly(); err != nil {
			// todo handle properly (abort, close, check disk space / resources, repopen)
			// todo combine with commit
			return 0, xerrors.Errorf("mark jbob read-only: %w", err)
		}
	}

	// 4. update head
	m.committedBlocks += int64(writeBlocks)
	m.committedSize += writeSize

	m.dblk.Lock()
	err = m.db.SetGroupHead(ctx, m.id, m.state, m.committedBlocks, m.committedSize, at)
	m.dblk.Unlock()
	if err != nil {
		// todo handle properly (retry, abort, close, check disk space / resources, repopen)
		return 0, xerrors.Errorf("update group head: %w", err)
	}

	// </todo async commit>

	return writeBlocks, nil
}

func (m *Group) Unlink(ctx context.Context, c []mh.Multihash) error {
	// write log

	// write idx

	// update head

	//TODO implement me
	panic("implement me")
}

func (m *Group) View(ctx context.Context, c []mh.Multihash, cb func(cidx int, data []byte)) error {
	// right now we just read from jbob
	return m.jb.View(c, func(cidx int, found bool, data []byte) error {
		// TODO: handle not found better?
		if !found {
			return xerrors.Errorf("group: block not found")
		}

		cb(cidx, data)
		return nil
	})
}

func (m *Group) Finalize(ctx context.Context) error {
	m.jblk.Lock()
	defer m.jblk.Unlock()

	if m.state != iface.GroupStateFull {
		return xerrors.Errorf("group not in state for finalization: %d", m.state)
	}

	if err := m.jb.MarkReadOnly(); err != nil && err != jbob.ErrReadOnly {
		return xerrors.Errorf("mark read-only: %w", err)
	}

	if err := m.jb.Finalize(); err != nil {
		return xerrors.Errorf("finalize jbob: %w", err)
	}

	if err := m.advanceState(ctx, iface.GroupStateBSSTExists); err != nil {
		return xerrors.Errorf("mark bsst exists: %w", err)
	}

	if err := m.jb.DropLevel(); err != nil {
		return xerrors.Errorf("removing leveldb index: %w", err)
	}

	if err := m.advanceState(ctx, iface.GroupStateLevelIndexDropped); err != nil {
		return xerrors.Errorf("mark level index dropped: %w", err)
	}

	return nil
}

func (m *Group) GenTopCar(ctx context.Context) error {
	m.jblk.RLock()
	defer m.jblk.RLock()

	if err := os.Mkdir(filepath.Join(m.path, "vcar"), 0755); err != nil {
		return xerrors.Errorf("make vcar dir: %w", err)
	}

	if m.state != iface.GroupStateLevelIndexDropped {
		return xerrors.Errorf("group not in state for generating top CAR: %d", m.state)
	}

	level := 1
	const arity = 2048
	var links []cid.Cid
	var nextLevelLinks []cid.Cid

	makeLinkBlock := func() (blocks.Block, error) {
		nd, err := cbor.WrapObject(links, mh.SHA2_256, -1)
		if err != nil {
			return nil, xerrors.Errorf("wrap links: %w", err)
		}

		links = links[:0]

		nextLevelLinks = append(nextLevelLinks, nd.Cid())

		return nd, nil
	}

	fname := filepath.Join(m.path, "vcar", fmt.Sprintf("layer%d.cardata", level))
	f, err := os.OpenFile(fname, os.O_CREATE|os.O_RDWR, 0644)
	outCdata := &cardata{
		f: f,
	}

	err = m.jb.Iterate(func(c mh.Multihash, data []byte) error {
		link := mhToRawCid(c)
		links = append(links, link)

		if len(links) == arity {
			bk, err := makeLinkBlock()
			if err != nil {
				return xerrors.Errorf("make link block: %w", err)
			}

			if err := outCdata.writeBlock(bk.Cid(), bk.RawData()); err != nil {
				return xerrors.Errorf("writing link block: %w", err)
			}
		}

		return nil
	})
	if err != nil {
		return xerrors.Errorf("iterate jbob: %w", err)
	}

	if len(links) > 0 {
		bk, err := makeLinkBlock()
		if err != nil {
			return xerrors.Errorf("make link block: %w", err)
		}

		if err := outCdata.writeBlock(bk.Cid(), bk.RawData()); err != nil {
			return xerrors.Errorf("writing link block: %w", err)
		}
	}
	if err := f.Close(); err != nil {
		return xerrors.Errorf("close level 1: %w", err)
	}

	for {
		level++
		fname := filepath.Join(m.path, "vcar", fmt.Sprintf("layer%d.cardata", level))
		f, err := os.OpenFile(fname, os.O_CREATE|os.O_RDWR, 0644)
		if err != nil {
			return xerrors.Errorf("open cardata file: %w", err)
		}

		outCdata = &cardata{
			f: f,
		}

		prevLevelLinks := nextLevelLinks

		// this actually works because nextLevelLinks grow slower than prevLevelLinks
		nextLevelLinks = nextLevelLinks[:0]

		for _, link := range prevLevelLinks {
			links = append(links, link)

			if len(links) == arity {
				bk, err := makeLinkBlock()
				if err != nil {
					return xerrors.Errorf("make link block: %w", err)
				}

				if err := outCdata.writeBlock(bk.Cid(), bk.RawData()); err != nil {
					return xerrors.Errorf("writing link block: %w", err)
				}
			}
		}

		if len(links) > 0 {
			bk, err := makeLinkBlock()
			if err != nil {
				return xerrors.Errorf("make link block: %w", err)
			}

			if err := outCdata.writeBlock(bk.Cid(), bk.RawData()); err != nil {
				return xerrors.Errorf("writing link block: %w", err)
			}
		}
		if err := f.Close(); err != nil {
			return xerrors.Errorf("close level %d: %w", level, err)
		}

		if len(prevLevelLinks) == 1 {
			break
		}
	}

	if err := os.WriteFile(filepath.Join(m.path, "vcar", "layers"), []byte(fmt.Sprintf("%d", level)), 0644); err != nil {
		return xerrors.Errorf("write layers file: %w", err)
	}

	if err := m.advanceState(ctx, iface.GroupStateVRCARDone); err != nil {
		return xerrors.Errorf("mark level index dropped: %w", err)
	}

	return nil
}

func (m *Group) advanceState(ctx context.Context, st iface.GroupState) error {
	m.dblk.Lock()
	defer m.dblk.Unlock()

	m.state = st

	// todo enter failed state on error
	return m.db.SetGroupState(ctx, m.id, st)
}

func (m *Group) Close() error {
	//TODO implement me
	panic("implement me")
}

func (m *Group) Sync(ctx context.Context) error {
	//TODO implement me
	panic("implement me")
}

type cardata struct {
	f *os.File
}

func (c *cardata) writeBlock(ci cid.Cid, data []byte) error {
	return carutil.LdWrite(c.f, ci.Bytes(), data)
}

func mhToRawCid(mh mh.Multihash) cid.Cid {
	return cid.NewCidV1(cid.Raw, mh)
}

var _ iface.Group = &Group{}
