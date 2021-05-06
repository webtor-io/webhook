package main

import (
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	cs "github.com/webtor-io/common-services"
	s "github.com/webtor-io/webhook/services"
)

func makeServeCMD() cli.Command {
	serveCmd := cli.Command{
		Name:    "serve",
		Aliases: []string{"s"},
		Usage:   "Serves web server",
		Action:  serve,
	}
	configureServe(&serveCmd)
	return serveCmd
}

func configureServe(c *cli.Command) {
	c.Flags = s.RegisterWebFlags([]cli.Flag{})
	c.Flags = cs.RegisterProbeFlags(c.Flags)
	c.Flags = s.RegisterPatreonFlags(c.Flags)
	c.Flags = cs.RegisterPGFlags(c.Flags)
}

func serve(c *cli.Context) error {
	// Setting DB
	db := cs.NewPG(c)
	defer db.Close()

	// Setting Migrations
	m := cs.NewPGMigration(db)
	err := m.Run()
	if err != nil {
		return err
	}

	// Setting ProbeService
	probe := cs.NewProbe(c)
	defer probe.Close()

	// Setting WebService
	web := s.NewWeb(c)
	defer web.Close()

	// Setting Patreon
	patreon := s.NewPatreon(c, db)

	// Registering Patreon handler
	web.RegisterProvider("/patreon", patreon.Handle)

	// Setting ServeService
	serve := cs.NewServe(probe, web)

	// And SERVE!
	err = serve.Serve()
	if err != nil {
		log.WithError(err).Error("Got server error")
	}
	return err
}
