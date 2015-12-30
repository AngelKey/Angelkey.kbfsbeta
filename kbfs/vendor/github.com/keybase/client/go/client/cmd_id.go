// Copyright 2015 Keybase, Inc. All rights reserved. Use of
// this source code is governed by the included BSD license.

package client

import (
	"fmt"

	"golang.org/x/net/context"

	"github.com/keybase/cli"
	"github.com/keybase/client/go/libcmdline"
	"github.com/keybase/client/go/libkb"
	keybase1 "github.com/keybase/client/go/protocol"
	rpc "github.com/keybase/go-framed-msgpack-rpc"
)

type CmdID struct {
	libkb.Contextified
	user           string
	trackStatement bool
	useDelegateUI  bool
}

func (v *CmdID) ParseArgv(ctx *cli.Context) error {
	nargs := len(ctx.Args())
	if nargs > 1 {
		return fmt.Errorf("Identify only takes one argument, the user to lookup.")
	}

	if nargs == 1 {
		v.user = ctx.Args()[0]
	}
	v.trackStatement = ctx.Bool("track-statement")
	v.useDelegateUI = ctx.Bool("delegate-identify-ui")
	return nil
}

func (v *CmdID) makeArg() keybase1.IdentifyArg {
	return keybase1.IdentifyArg{
		UserAssertion:  v.user,
		TrackStatement: v.trackStatement,
		UseDelegateUI:  v.useDelegateUI,
		Reason:         keybase1.IdentifyReason{Reason: "CLI id command"},
	}
}

func (v *CmdID) Run() error {
	var cli keybase1.IdentifyClient
	protocols := []rpc.Protocol{}

	// always register this, even if ui is delegated, so that
	// fallback to terminal UI works.
	protocols = append(protocols, NewIdentifyUIProtocol(v.G()))
	cli, err := GetIdentifyClient(v.G())
	if err != nil {
		return err
	}
	if err := RegisterProtocolsWithContext(protocols, v.G()); err != nil {
		return err
	}

	arg := v.makeArg()
	_, err = cli.Identify(context.TODO(), arg)
	if _, ok := err.(libkb.SelfNotFoundError); ok {
		msg := `Could not find UID or username for you on this device.
You can either specify a user to id: keybase id <username>
Or log in once on this device and run "keybase id" again.
`
		v.G().UI.GetDumbOutputUI().Printf(msg)
		return nil
	}
	return err
}

func NewCmdID(cl *libcmdline.CommandLine, g *libkb.GlobalContext) cli.Command {
	ret := cli.Command{
		Name:         "id",
		ArgumentHelp: "[username]",
		Usage:        "Identify a user and check their signature chain",
		Description:  "Identify a user and check their signature chain.  Don't specify a username to identify yourself.  You can also specify proof assertions like user@twitter.",
		Flags: []cli.Flag{
			cli.BoolFlag{
				Name:  "t, track-statement",
				Usage: "Output a tracking statement (in JSON format).",
			},
		},
		Action: func(c *cli.Context) {
			cl.ChooseCommand(NewCmdIDRunner(g), "id", c)
		},
	}
	cmdIDAddFlags(&ret)
	return ret
}

func NewCmdIDRunner(g *libkb.GlobalContext) *CmdID {
	return &CmdID{Contextified: libkb.NewContextified(g)}
}

func (v *CmdID) SetUser(s string) {
	v.user = s
}

func (v *CmdID) UseDelegateUI() {
	v.useDelegateUI = true
}

func (v *CmdID) GetUsage() libkb.Usage {
	return libkb.Usage{
		Config:    true,
		KbKeyring: true,
		API:       true,
	}
}
