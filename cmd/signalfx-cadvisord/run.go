package main

import (
	"net/url"
	"time"

	"github.com/codegangsta/cli"
	"github.com/michaeltrobinson/cadvisor-integration/scraper"
	"github.com/signalfx/metricproxy/protocol/signalfx"

	log "github.com/Sirupsen/logrus"
)

var (
	sfxAPIToken       string
	sfxIngestURL      string
	clusterName       string
	sendInterval      time.Duration
	cadvisorPort      int
	discoveryInterval time.Duration
	maxDatapoints     int
	kubeUser          string
	kubePass          string
)

func init() {
	app.Commands = append(app.Commands, cli.Command{
		Name:   "run",
		Usage:  "start the service (the default)",
		Action: run,
		Before: setupRun,
		Flags: []cli.Flag{
			cli.StringFlag{
				Name:   "sfx-ingest-url",
				EnvVar: "SFX_ENDPOINT",
				Value:  "https://ingest.signalfx.com",
				Usage:  "SignalFx ingest URL",
			},
			cli.StringFlag{
				Name:   "sfx-api-token",
				EnvVar: "SFX_API_TOKEN",
				Usage:  "SignalFx API token",
			},
			cli.StringFlag{
				Name:   "cluster-name",
				EnvVar: "CLUSTER_NAME",
				Usage:  "Cluster name will appear as dimension",
			},
			cli.DurationFlag{
				Name:   "send-interval",
				EnvVar: "SEND_INTERVAL",
				Value:  time.Second * 30,
				Usage:  "Rate at which data is queried from cAdvisor and send to SignalFx",
			},
			cli.IntFlag{
				Name:   "cadvisor-port",
				EnvVar: "CADVISOR_PORT",
				Value:  4194,
				Usage:  "Port on which cAdvisor listens",
			},
			cli.DurationFlag{
				Name:   "discovery-interval",
				EnvVar: "NODE_SERVICE_DISCOVERY_INTERVAL",
				Value:  time.Minute * 5,
				Usage:  "Rate at which nodes and services will be rediscovered",
			},
			cli.StringFlag{
				Name:   "kube-user",
				EnvVar: "KUBE_USER",
				Usage:  "Username to authenticate to kubernetes api",
			},
			cli.StringFlag{
				Name:   "kube-pass",
				EnvVar: "KUBE_PASS",
				Usage:  "Password to authenticate to kubernetes api",
			},
			cli.IntFlag{
				Name:   "max-datapoints",
				EnvVar: "MAX_DATAPOINTS",
				Value:  50,
				Usage:  "How many datapoints to batch before forwarding to SignalFX",
			},
		},
	})
}

func setupRun(c *cli.Context) error {
	sfxAPIToken = c.String("sfx-api-token")
	if sfxAPIToken == "" {
		cli.ShowAppHelp(c)
		log.Fatal("API token is required")
	}

	clusterName = c.String("cluster-name")
	if clusterName == "" {
		cli.ShowAppHelp(c)
		log.Fatal("cluster name is required")
	}

	sfxIngestURL = c.String("sfx-ingest-url")
	sendInterval = c.Duration("send-interval")
	cadvisorPort = c.Int("cadvisor-port")
	discoveryInterval = c.Duration("discovery-interval")

	kubeUser = c.String("kube-user")
	kubePass = c.String("kube-pass")
	if kubeUser == "" || kubePass == "" {
		cli.ShowAppHelp(c)
		log.Fatal("kubernetes credentials are required")
	}
	maxDatapoints = c.Int("max-datapoints")
	return nil
}

func run(c *cli.Context) {
	s := scraper.New(
		newSfxClient(sfxIngestURL, sfxAPIToken),
		scraper.Config{
			ClusterName:   clusterName,
			CadvisorPort:  cadvisorPort,
			KubeUser:      kubeUser,
			KubePass:      kubePass,
			MaxDatapoints: maxDatapoints,
		})

	if err := s.Run(sendInterval, discoveryInterval); err != nil {
		log.WithError(err).Fatal("failure")
	}
}

func newSfxClient(ingestURL, authToken string) *signalfx.Forwarder {
	sfxEndpoint, err := url.Parse(ingestURL)
	if err != nil {
		panic("failed to parse SFX ingest URL")
	}
	return signalfx.NewSignalfxJSONForwarder(sfxEndpoint.String(), time.Second*10, authToken, 10, "", "", "")
}
