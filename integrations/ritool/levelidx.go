package main

import (
	"fmt"
	"github.com/multiformats/go-multihash"

	"github.com/ipfs/go-cid"
	"github.com/lotus-web3/ribs/carlog"
	"github.com/urfave/cli/v2"
	"golang.org/x/xerrors"
)

var idxLevelCmd = &cli.Command{
	Name:  "idx-level",
	Usage: "Head commands",
	Subcommands: []*cli.Command{
		toTruncateCmd,
	},
}

var toTruncateCmd = &cli.Command{
	Name:      "to-truncate",
	Usage:     "read a head file into a json file",
	ArgsUsage: "[leveldb file]",
	Flags: []cli.Flag{
		&cli.Int64Flag{
			Name:     "carlog-size",
			Required: true,
		},
		&cli.BoolFlag{
			Name: "cids",
		},
	},
	Action: func(c *cli.Context) error {
		if c.NArg() != 1 {
			return cli.Exit("Invalid number of arguments", 1)
		}

		li, err := carlog.OpenLevelDBIndex(c.Args().First(), false)
		if err != nil {
			return xerrors.Errorf("open leveldb index: %w", err)
		}

		mhs, err := li.ToTruncate(c.Int64("carlog-size"))
		if err != nil {
			return xerrors.Errorf("to truncate: %w", err)
		}

		if !c.Bool("cids") {
			fmt.Println("blocks to truncate:", len(mhs))
		}

		for _, mh := range mhs {
			fmt.Println(cid.NewCidV1(cid.Raw, mh).String())
		}

		return nil
	},
}

var findCmd = &cli.Command{
	Name:      "find",
	Usage:     "get index entry for a cid",
	ArgsUsage: "[leveldb file] [cid]",
	Action: func(c *cli.Context) error {
		if c.NArg() != 2 {
			return cli.Exit("Invalid number of arguments", 1)
		}

		li, err := carlog.OpenLevelDBIndex(c.Args().First(), false)
		if err != nil {
			return xerrors.Errorf("open leveldb index: %w", err)
		}

		ci, err := cid.Parse(c.Args().Get(1))
		if err != nil {
			return xerrors.Errorf("parse cid: %w", err)
		}

		offs, err := li.Get([]multihash.Multihash{ci.Hash()})
		if err != nil {
			return xerrors.Errorf("get: %w", err)
		}

		for _, off := range offs {
			fmt.Println(off)
		}

		return nil
	},
}
