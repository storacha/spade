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
	cmdName := app.AppName + "-cron"
	log := logging.Logger(fmt.Sprintf("%s(%d)", cmdName, os.Getpid()))
	logging.SetLogLevel("*", "INFO")

	home, err := os.UserHomeDir()
	if err != nil {
		log.Error(cmn.WrErr(err))
		os.Exit(1)
	}

	(&ufcli.UFcli{
		Logger:   log,
		TOMLPath: fmt.Sprintf("%s/%s.toml", home, app.AppName),
		AppConfig: ufcli.App{
			Name:  cmdName,
			Usage: "Misc background processes for " + app.AppName,
			Commands: []*ufcli.Command{
				pollProviders,
				trackDeals,
				signPending,
				proposePending,
				bulkPiecePoll,
				updateCurrentF05CollateralEstimate,
			},
			Flags: app.CommonFlags,
		},
		GlobalInit: app.GlobalInit,
	}).RunAndExit(context.Background())
}
