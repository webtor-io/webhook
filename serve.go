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
	c.Flags = s.RegisterNowPaymentsFlags(c.Flags)
	c.Flags = s.RegisterLavatopFlags(c.Flags)
	c.Flags = cs.RegisterNATSFlags(c.Flags)
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

	// Setting NATS
	nats := cs.NewNATS(c)
	if nats != nil {
		defer nats.Close()
	}

	// Setting ProbeService
	probe := cs.NewProbe(c)
	defer probe.Close()

	// Setting WebService
	web := s.NewWeb(c)
	defer web.Close()

	// Setting Patreon
	patreon := s.NewPatreon(c, db, nats)
	defer patreon.Close()

	// Registering Patreon handler
	web.RegisterProvider("/patreon", patreon.Handle)

	// Setting NOWPayments
	np := s.NewNowPayments(c, db, nats)
	defer np.Close()

	// Registering the NOWPayments IPN receiver (public, like /patreon)
	web.RegisterProvider("/nowpayments", np.HandleIPN)

	// Setting lava.top (storefront purchases: webhooks only, no invoice API)
	lt := s.NewLavatop(c, db, nats)
	defer lt.Close()

	// Registering the lava.top webhook receiver (public, like /patreon)
	web.RegisterProvider("/lavatop", lt.HandleWebhook)

	// Background check that the storefront offers still resolve to tiers
	lt.CheckOffers()

	// Setting the provider-agnostic invoice API (cluster-only: the ingress
	// path whitelist is what keeps these routes off the internet)
	invoice := s.NewInvoice(db, map[string]s.InvoiceProvider{
		"nowpayments": np,
	})
	invoice.RegisterHandlers(web)

	// Setting ServeService
	serve := cs.NewServe(probe, web)

	// And SERVE!
	err = serve.Serve()
	if err != nil {
		log.WithError(err).Error("got server error")
	}
	return err
}
