package main

import (
	"context"

	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	cs "github.com/webtor-io/common-services"
	s "github.com/webtor-io/webhook/services"
)

func makeRefreshMembersCMD() cli.Command {
	cmd := cli.Command{
		Name:   "refresh-members",
		Usage:  "Refreshes patreon.member materialized view",
		Action: refreshMembers,
	}
	cmd.Flags = s.RegisterPatreonFlags(cs.RegisterPGFlags([]cli.Flag{}))
	return cmd
}

func refreshMembers(c *cli.Context) error {
	db := cs.NewPG(c)
	defer db.Close()

	p := s.NewPatreon(c, db, nil)
	defer p.Close()

	if err := p.RefreshMembers(context.Background()); err != nil {
		log.WithError(err).Error("failed to refresh patreon.member")
		return err
	}
	log.Info("patreon.member refreshed")
	return nil
}
