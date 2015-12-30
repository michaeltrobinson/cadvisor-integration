package scraper

import (
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"runtime"
	"syscall"
	"time"

	"github.com/goinggo/workpool"
	"github.com/google/cadvisor/client"
	"github.com/google/cadvisor/metrics"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/signalfx/golib/datapoint"
	"github.com/signalfx/metricproxy/protocol/signalfx"

	kubeAPI "k8s.io/kubernetes/pkg/api"
	kube "k8s.io/kubernetes/pkg/client/unversioned"
	kubeFields "k8s.io/kubernetes/pkg/fields"
	kubeLabels "k8s.io/kubernetes/pkg/labels"

	log "github.com/Sirupsen/logrus"
)

var (
	re             = regexp.MustCompile(`^k8s_(?P<kubernetes_container_name>[^_\.]+)[^_]+_(?P<kubernetes_pod_name>[^_]+)_(?P<kubernetes_namespace>[^_]+)`)
	reCaptureNames = re.SubexpNames()
)

// Config for Scraper
type Config struct {
	ClusterName   string
	CadvisorPort  int
	KubeUser      string
	KubePass      string
	MaxDatapoints int
}

// Scraper scrapes cadvisor for metrics and forwards them to SignalFX
type Scraper struct {
	forwarder    *signalfx.Forwarder
	cfg          Config
	cadvisorURLs []string
}

// New creates a new Scraper with the given configuration
func New(forwarder *signalfx.Forwarder, cfg Config) *Scraper {
	return &Scraper{
		forwarder: forwarder,
		cfg:       cfg,
	}
}

// Run starts this Scraper, blocks until it finishes or catches a signal
func (p *Scraper) Run(forwardInterval, discoverInterval time.Duration) (err error) {
	podToServiceMap := p.getServices()
	hostIPtoNameMap, nodeIPs := p.getNodes(p.cfg.CadvisorPort)
	p.cadvisorURLs = nodeIPs

	cadvisorServers := make([]*url.URL, len(p.cadvisorURLs))
	for i, serverURL := range p.cadvisorURLs {
		cadvisorServers[i], err = url.Parse(serverURL)
		if err != nil {
			return err
		}
	}

	log.Info("scraper started")
	workSet := newWorkSet(p.forwarder, p.cfg.MaxDatapoints, p.cfg.ClusterName)
	stop := make(chan struct{}, 1)

	workSet.setPodToServiceMap(podToServiceMap)
	workSet.setHostIPtoNameMap(hostIPtoNameMap)

	workSet.buildWorkList(p.cadvisorURLs)

	// Wait on channel input and forward datapoints to SignalFx
	go func() {
		workSet.waitAndForward() // Blocking call!
		stop <- struct{}{}       // Stop
	}()

	workPool := workpool.New(runtime.NumCPU(), int32(len(p.cadvisorURLs)+1))

	forwardTicker := time.NewTicker(forwardInterval)
	go func() {
		for range forwardTicker.C {
			workSet.forEachWork(func(i int, w *work) bool {
				if err := workPool.PostWork("", w); err != nil {
					log.WithError(err).Error("failed to post work to work pool")
				}
				return true
			})
		}
	}()

	discoverTicker := time.NewTicker(discoverInterval)
	go func() {
		for range discoverTicker.C {
			podMap := p.getServices()
			hostMap, _ := p.getNodes(p.cfg.CadvisorPort)
			hostMapCopy := make(map[string]kubeAPI.Node)
			for k, v := range hostMap {
				hostMapCopy[k] = v
			}

			// Remove known nodes
			workSet.forEachWork(func(i int, w *work) bool {
				delete(hostMapCopy, w.serverURL)
				return true
			})

			if len(hostMapCopy) != 0 {
				workSet.setHostIPtoNameMap(hostMap)
				// Add new(remaining) nodes to monitoring
				for serverURL := range hostMapCopy {
					cadvisorClient, localERR := client.NewClient(serverURL)
					if localERR != nil {
						log.WithError(localERR).Error("failed connect to cadvisor server")
						continue
					}

					workSet.addWork(&work{
						serverURL: serverURL,
						collector: metrics.NewPrometheusCollector(&cadvisorInfoProvider{
							cc: cadvisorClient,
						}, nameToLabel),
						chRecvOnly: make(chan prometheus.Metric),
					})
				}
			} else {
				log.Info("no new nodes appeared")
			}
			workSet.setPodToServiceMap(podMap)
		}
	}()

	go func() {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		// Wait for signal
		sig := <-sigs
		log.WithField("sig", sig).Info("caught signal")
		stop <- struct{}{} // Stop
	}()

	<-stop // Block here till stopped
	discoverTicker.Stop()
	forwardTicker.Stop()
	log.Info("scraper stopped")
	return
}

func (p *Scraper) getNodes(cPort int) (hostIPtoNodeMap map[string]kubeAPI.Node, nodeIPs []string) {
	kubeClient, err := p.kubeClient()
	if err != nil {
		log.WithError(err).Error("failed to create kube client")
		return nil, nil
	}

	hostIPtoNodeMap = make(map[string]kubeAPI.Node, 2)
	nodeIPs = make([]string, 0, 2)
	nodeList, apiErr := kubeClient.Nodes().List(kubeLabels.Everything(), kubeFields.Everything())
	if apiErr != nil {
		log.WithError(apiErr).Error("failed to get node list")
	} else {
		log.WithField("num", len(nodeList.Items)).Info("found nodes")
		for _, node := range nodeList.Items {
			var hostIP string
			for _, nodeAddress := range node.Status.Addresses {
				switch nodeAddress.Type {
				case kubeAPI.NodeInternalIP:
					hostIP = nodeAddress.Address
					break
				case kubeAPI.NodeLegacyHostIP:
					hostIP = nodeAddress.Address
				}
			}
			if hostIP != "" {
				hostIP = fmt.Sprintf("http://%v:%v", hostIP, cPort)
				nodeIPs = append(nodeIPs, hostIP)
				hostIPtoNodeMap[hostIP] = node
			}
		}
	}

	return hostIPtoNodeMap, nodeIPs
}

func (p *Scraper) getServices() (podToServiceMap map[string]string) {
	kubeClient, err := p.kubeClient()
	if err != nil {
		log.WithError(err).Error("failed to create kube client")
		return nil
	}

	serviceList, apiErr := kubeClient.Services("").List(kubeLabels.Everything())
	if apiErr != nil {
		log.WithError(apiErr).Error("failed to get service list")
		return nil
	}
	log.WithField("num", len(serviceList.Items)).Info("found services")

	podToServiceMap = make(map[string]string, 2)
	for _, service := range serviceList.Items {
		podList, apiErr := kubeClient.Pods("").List(kubeLabels.SelectorFromSet(service.Spec.Selector), kubeFields.Everything())
		if apiErr != nil {
			log.WithError(apiErr).Error("failed to get pod list")
		} else {
			log.WithFields(log.Fields{
				"service": service.ObjectMeta.Name,
				"num":     len(podList.Items),
			}).Info("found pods")
			for _, pod := range podList.Items {
				podToServiceMap[pod.ObjectMeta.Name] = service.ObjectMeta.Name
			}
		}
	}
	return podToServiceMap
}

func (p *Scraper) kubeClient() (*kube.Client, error) {
	cfg, err := kube.InClusterConfig()
	if err != nil {
		return nil, err
	}
	cfg.BearerToken = ""
	cfg.Username = p.cfg.KubeUser
	cfg.Password = p.cfg.KubePass
	log.WithField("host", cfg.Host).Info("connecting to kube api")
	client, err := kube.New(cfg)
	if err != nil {
		return nil, err
	}
	return client, nil
}

type sortableDatapoint []*datapoint.Datapoint

func (sd sortableDatapoint) Len() int {
	return len(sd)
}

func (sd sortableDatapoint) Swap(i, j int) {
	sd[i], sd[j] = sd[j], sd[i]
}

func (sd sortableDatapoint) Less(i, j int) bool {
	return sd[i].Timestamp.Unix() < sd[j].Timestamp.Unix()
}

func nameToLabel(name string) map[string]string {
	extraLabels := map[string]string{}
	matches := re.FindStringSubmatch(name)
	for i, match := range matches {
		if len(reCaptureNames[i]) > 0 {
			extraLabels[re.SubexpNames()[i]] = match
		}
	}
	return extraLabels
}
