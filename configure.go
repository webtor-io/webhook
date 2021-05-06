package main

import (
	"github.com/urfave/cli"
	cs "github.com/webtor-io/common-services"
)

func configure(app *cli.App) {
	pgMigrationCmd := cs.MakePGMigrationCMD()
	serveCmd := makeServeCMD()
	app.Commands = []cli.Command{serveCmd, pgMigrationCmd}
}
