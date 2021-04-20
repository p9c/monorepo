package main

import (
	"bytes"
	"encoding/json"
	"os"

	"github.com/p9c/qu"

	_ "gioui.org/app/permission/networkstate" // todo: integrate this into routeable package
	_ "gioui.org/app/permission/storage"      // this enables the home folder appdata directory to work on android (and ios)
	"github.com/p9c/log"
	"github.com/p9c/matrjoska/pod/config"
	"github.com/p9c/matrjoska/pod/podcfgs"
	"github.com/p9c/matrjoska/pod/state"
	"github.com/p9c/matrjoska/version"

	// This ensures the database drivers get registered
	_ "github.com/p9c/matrjoska/pkg/database/ffldb"

	// _ "gioui.org/app/permission/bluetooth"
	// _ "gioui.org/app/permission/camera"
)

func main() {
	<-Main()
}

func Main() (quit qu.C) {
	quit=qu.T()
	go func() {
		log.SetLogLevel("trace")
		T.Ln(version.Get())
		var cx *state.State
		var e error
		if cx, e = state.GetNew(podcfgs.GetDefaultConfig(), quit); E.Chk(e) {
			fail()
		}

		// fail()
		if e = debugConfig(cx.Config); E.Chk(e) {
		}

		D.Ln("running command", cx.Config.RunningCommand.Name)
		if e = cx.Config.RunningCommand.Entrypoint(cx); E.Chk(e) {
			fail()
		}
	}()
	return quit
}

func fail() {
	os.Exit(1)
}

func debugConfig(c *config.Config) (e error) {
	c.ShowAll = true
	defer func() { c.ShowAll = false }()
	var j []byte
	if j, e = c.MarshalJSON(); E.Chk(e) {
		return
	}
	var b []byte
	jj := bytes.NewBuffer(b)
	if e = json.Indent(jj, j, "", "\t"); E.Chk(e) {
		return
	}
	// T.Ln("\n"+jj.String())
	return
}
