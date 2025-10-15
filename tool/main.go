package main

import (
	"context"
	"fmt"
	"os"

	"code.riba.cloud/go/toolbox/cmn"
	"code.riba.cloud/go/toolbox/ufcli"
	logging "github.com/ipfs/go-log/v2"
	"github.com/storacha/spade/internal/app"
)

func main() {

	cmdName := app.AppName + "-tool"
	log := logging.Logger(fmt.Sprintf("%s(%d)", cmdName, os.Getpid()))

	// *always* log json
	{
		lcfg := logging.GetConfig()
		lcfg.Format = logging.JSONOutput
		logging.SetupLogging(lcfg)
		logging.SetLogLevel("*", "INFO")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		log.Error(cmn.WrErr(err))
		os.Exit(1)
	}
	(&ufcli.UFcli{
		Logger:              log,
		TOMLPath:            fmt.Sprintf("%s/%s.toml", home, app.AppName),
		AllowConcurrentRuns: true,
		AppConfig: ufcli.App{
			Name:  cmdName,
			Usage: "admin cli tool for debugging spade",
			Commands: []*ufcli.Command{
				pieceManifestCmd,
			},
			Flags: app.CommonFlags,
		},
		GlobalInit: app.GlobalInit,
	}).RunAndExit(context.Background())
}
