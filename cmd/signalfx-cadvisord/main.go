package main

import (
	"os"

	log "github.com/Sirupsen/logrus"
	"github.com/codegangsta/cli"
)

const (
	version = "0.1.0"
	name    = "signalfx-cadvisord"
)

var app = initApp()

// runs before init
func initApp() *cli.App {
	app := cli.NewApp()

	app.Name = name
	app.Version = version
	app.Usage = "scrapes metrics from cAdvisor and forwards them to SignalFx"
	app.Commands = []cli.Command{}
	app.Authors = []cli.Author{
		{Name: "Michael Robinson", Email: "mrobinson@zvelo.com"},
	}

	return app
}

func main() {
	if err := app.Run(os.Args); err != nil {
		log.WithError(err).Fatal("app returned error")
	}
}
