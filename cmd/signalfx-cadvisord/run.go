package main

import (
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/codegangsta/cli"
	"github.com/goinggo/workpool"
	"github.com/google/cadvisor/client"
	"github.com/google/cadvisor/metrics"
	"github.com/michaeltrobinson/cadvisor-integration/scrapper"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/signalfx/golib/datapoint"
	"github.com/signalfx/metricproxy/protocol/signalfx"
	"golang.org/x/net/context"

	kubeAPI "k8s.io/kubernetes/pkg/api"
	kube "k8s.io/kubernetes/pkg/client/unversioned"
	kubeFields "k8s.io/kubernetes/pkg/fields"
	kubeLabels "k8s.io/kubernetes/pkg/labels"

	log "github.com/Sirupsen/logrus"
	info "github.com/google/cadvisor/info/v1"
	dto "github.com/prometheus/client_model/go"
)

const maxDatapoints = 50

var (
	re             *regexp.Regexp
	reCaptureNames []string

	sfxAPIToken       string
	sfxIngestURL      string
	clusterName       string
	sendInterval      time.Duration
	cadvisorPort      int
	discoveryInterval time.Duration

	kubeUser string
	kubePass string
)

// Config for prometheusScraper
type Config struct {
	IngestURL              string
	CadvisorURL            []string
	APIToken               string
	DataSendRate           time.Duration
	ClusterName            string
	NodeServiceRefreshRate time.Duration
	CadvisorPort           int
}

type prometheusScraper struct {
	forwarder *signalfx.Forwarder
	cfg       *Config
}

type scrapWork2 struct {
	serverURL  string
	collector  *metrics.PrometheusCollector
	chRecvOnly chan prometheus.Metric
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

type cadvisorInfoProvider struct {
	cc *client.Client
}

func (cip *cadvisorInfoProvider) SubcontainersInfo(containerName string, query *info.ContainerInfoRequest) ([]*info.ContainerInfo, error) {
	infos, err := cip.cc.SubcontainersInfo(containerName, &info.ContainerInfoRequest{NumStats: 3})
	if err != nil {
		return nil, err
	}
	ret := make([]*info.ContainerInfo, len(infos))
	for i, info := range infos {
		containerInfo := info
		ret[i] = &containerInfo
	}
	return ret, nil
}

func (cip *cadvisorInfoProvider) GetVersionInfo() (*info.VersionInfo, error) {
	//TODO: remove fake info
	return &info.VersionInfo{
		KernelVersion:      "4.1.6-200.fc22.x86_64",
		ContainerOsVersion: "Fedora 22 (Twenty Two)",
		DockerVersion:      "1.8.1",
		CadvisorVersion:    "0.16.0",
		CadvisorRevision:   "abcdef",
	}, nil
}

func (cip *cadvisorInfoProvider) GetMachineInfo() (*info.MachineInfo, error) {
	return cip.cc.MachineInfo()
}

func (scrapWork *scrapWork2) DoWork(workRoutine int) {
	scrapWork.collector.Collect(scrapWork.chRecvOnly)
}

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
				Usage:  "API token",
			},
			cli.StringFlag{
				Name:   "cluster-name",
				EnvVar: "CLUSTER_NAME",
				Usage:  "Cluster name will appear as dimension",
			},
			cli.DurationFlag{
				Name:   "send-interval",
				EnvVar: "SEND_INTERVAL",
				Value:  time.Second * 1,
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
		},
	})
}

func setupRun(c *cli.Context) error {
	re = regexp.MustCompile(`^k8s_(?P<kubernetes_container_name>[^_\.]+)[^_]+_(?P<kubernetes_pod_name>[^_]+)_(?P<kubernetes_namespace>[^_]+)`)
	reCaptureNames = re.SubexpNames()

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
	return nil
}

func run(c *cli.Context) {
	var instance = prometheusScraper{
		forwarder: newSfxClient(sfxIngestURL, sfxAPIToken),
		cfg: &Config{
			IngestURL:              sfxIngestURL,
			APIToken:               sfxAPIToken,
			DataSendRate:           sendInterval,
			ClusterName:            clusterName,
			NodeServiceRefreshRate: discoveryInterval,
			CadvisorPort:           cadvisorPort,
		},
	}

	if err := instance.main(sendInterval, discoveryInterval); err != nil {
		log.WithError(err).Fatal("failure")
	}
}

func getMapKeys(m map[string]time.Duration) (keys []string) {
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func newSfxClient(ingestURL, authToken string) *signalfx.Forwarder {
	return signalfx.NewSignalfxJSONForwarder(strings.Join([]string{ingestURL, "v2/datapoint"}, "/"), time.Second*10, authToken, 10, "", "", "")
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

func replaceAuth(kubeCfg *kube.Config) {
	kubeCfg.BearerToken = ""
	kubeCfg.Username = kubeUser
	kubeCfg.Password = kubePass
}

func updateNodes(cPort int) (hostIPtoNodeMap map[string]kubeAPI.Node, nodeIPs []string) {
	kubeCfg, err := kube.InClusterConfig()
	if err != nil {
		log.WithError(err).Error("failed to create kube config")
		return nil, nil
	}
	replaceAuth(kubeCfg)
	log.WithField("host", kubeCfg.Host).Info("connecting to kube api")
	kubeClient, kubeErr := kube.New(kubeCfg)
	if kubeErr != nil {
		log.WithError(kubeErr).Error("failed to create kube client")
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

func updateServices() (podToServiceMap map[string]string) {
	kubeCfg, err := kube.InClusterConfig()
	if err != nil {
		log.WithError(err).Error("failed to create kube config")
		return nil
	}
	replaceAuth(kubeCfg)
	log.WithField("host", kubeCfg.Host).Info("connecting to kube api")
	kubeClient, kubeErr := kube.New(kubeCfg)
	if kubeErr != nil {
		log.WithError(kubeErr).Error("failed to create kube client")
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

func (p *prometheusScraper) main(paramDataSendRate, paramNodeServiceDiscoveryRate time.Duration) (err error) {
	podToServiceMap := updateServices()
	hostIPtoNameMap, nodeIPs := updateNodes(p.cfg.CadvisorPort)
	p.cfg.CadvisorURL = nodeIPs

	cadvisorServers := make([]*url.URL, len(p.cfg.CadvisorURL))
	for i, serverURL := range p.cfg.CadvisorURL {
		cadvisorServers[i], err = url.Parse(serverURL)
		if err != nil {
			return err
		}
	}

	log.Info("scraper started")
	scrapWorkCache := newScrapWorkCache(p.cfg, p.forwarder)
	stop := make(chan error, 1)

	scrapWorkCache.setPodToServiceMap(podToServiceMap)
	scrapWorkCache.setHostIPtoNameMap(hostIPtoNameMap)

	scrapWorkCache.buildWorkList(p.cfg.CadvisorURL)

	// Wait on channel input and forward datapoints to SignalFx
	go func() {
		scrapWorkCache.waitAndForward()                // Blocking call!
		stop <- errors.New("all channels were closed") // Stop all timers
	}()

	workPool := workpool.New(runtime.NumCPU(), int32(len(p.cfg.CadvisorURL)+1))

	scrapWorkTicker := time.NewTicker(paramDataSendRate)
	go func() {
		for range scrapWorkTicker.C {
			scrapWorkCache.foreachWork(func(i int, w *scrapWork2) bool {
				workPool.PostWork("", w)
				return true
			})
		}
	}()

	updateNodeAndPodTimer := time.NewTicker(paramNodeServiceDiscoveryRate)
	go func() {
		for range updateNodeAndPodTimer.C {

			podMap := updateServices()
			hostMap, _ := updateNodes(p.cfg.CadvisorPort)

			hostMapCopy := make(map[string]kubeAPI.Node)
			for k, v := range hostMap {
				hostMapCopy[k] = v
			}

			// Remove known nodes
			scrapWorkCache.foreachWork(func(i int, w *scrapWork2) bool {
				delete(hostMapCopy, w.serverURL)
				return true
			})

			if len(hostMapCopy) != 0 {
				scrapWorkCache.setHostIPtoNameMap(hostMap)

				// Add new(remaining) nodes to monitoring
				for serverURL := range hostMapCopy {
					cadvisorClient, localERR := client.NewClient(serverURL)
					if localERR != nil {
						log.WithError(localERR).Error("failed connect to cadvisor server")
						continue
					}

					scrapWorkCache.addWork(&scrapWork2{
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

			scrapWorkCache.setPodToServiceMap(podMap)
		}
	}()

	err = <-stop // Block here till stopped

	updateNodeAndPodTimer.Stop()
	scrapWorkTicker.Stop()

	return
}

type scrapWorkCache struct {
	workCache       []*scrapWork2
	cases           []reflect.SelectCase
	podToServiceMap map[string]string
	hostIPtoNameMap map[string]kubeAPI.Node
	forwarder       *signalfx.Forwarder
	cfg             *Config
	mutex           *sync.Mutex
}

func newScrapWorkCache(cfg *Config, forwarder *signalfx.Forwarder) *scrapWorkCache {
	return &scrapWorkCache{
		workCache: make([]*scrapWork2, 0, 1),
		cases:     make([]reflect.SelectCase, 0, 1),
		forwarder: forwarder,
		cfg:       cfg,
		mutex:     &sync.Mutex{},
	}
}

func (swc *scrapWorkCache) addWork(work *scrapWork2) {
	swc.mutex.Lock()
	defer swc.mutex.Unlock()

	swc.workCache = append(swc.workCache, work)
	c := reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(work.chRecvOnly)}
	swc.cases = append(swc.cases, c)
}

// Build list of work
func (swc *scrapWorkCache) buildWorkList(URLList []string) {
	for _, serverURL := range URLList {
		cadvisorClient, localERR := client.NewClient(serverURL)
		if localERR != nil {
			log.WithError(localERR).Error("failed connect to cadvisor server")
			continue
		}

		swc.addWork(&scrapWork2{
			serverURL: serverURL,
			collector: metrics.NewPrometheusCollector(&cadvisorInfoProvider{
				cc: cadvisorClient,
			}, nameToLabel),
			chRecvOnly: make(chan prometheus.Metric),
		})
	}
}

func (swc *scrapWorkCache) setPodToServiceMap(m map[string]string) {
	swc.mutex.Lock()
	defer swc.mutex.Unlock()

	swc.podToServiceMap = m
}

func (swc *scrapWorkCache) setHostIPtoNameMap(m map[string]kubeAPI.Node) {
	swc.mutex.Lock()
	defer swc.mutex.Unlock()

	swc.hostIPtoNameMap = m
}

type eachWorkFunc func(int, *scrapWork2) bool

func (swc *scrapWorkCache) foreachWork(f eachWorkFunc) {
	swc.mutex.Lock()
	workCacheCopy := make([]*scrapWork2, len(swc.workCache))
	copy(workCacheCopy, swc.workCache)
	swc.mutex.Unlock()

	for index, work := range workCacheCopy {
		if !f(index, work) {
			return
		}
	}
}

func (swc *scrapWorkCache) fillNodeDims(chosen int, dims map[string]string) {

	node, ok := func() (n kubeAPI.Node, b bool) {
		swc.mutex.Lock()
		defer func() {
			swc.mutex.Unlock()
			if r := recover(); r != nil {
				log.WithField("r", r).Info("recovered in fillNodeDims")
			}
		}()

		n, b = swc.hostIPtoNameMap[swc.workCache[chosen].serverURL]
		return
	}()

	if ok {
		dims["node"] = node.ObjectMeta.Name
		dims["node_container_runtime_version"] = node.Status.NodeInfo.ContainerRuntimeVersion
		dims["node_kernel_version"] = node.Status.NodeInfo.KernelVersion
		dims["node_kubelet_version"] = node.Status.NodeInfo.KubeletVersion
		dims["node_os_image"] = node.Status.NodeInfo.OsImage
		dims["node_kubeproxy_version"] = node.Status.NodeInfo.KubeProxyVersion
	}
}

// Wait on channel input and forward datapoints to SignalFx.
// This function will block.
func (swc *scrapWorkCache) waitAndForward() {
	swc.mutex.Lock()
	remaining := len(swc.cases)
	swc.mutex.Unlock()

	ctx := context.Background()

	i := 0
	ret := make([]*datapoint.Datapoint, maxDatapoints)
	for remaining > 0 {
		chosen, value, ok := reflect.Select(swc.cases)
		if !ok {
			// The chosen channel has been closed, so zero out the channel to disable the case
			swc.mutex.Lock()
			swc.cases[chosen].Chan = reflect.ValueOf(nil)
			swc.cases = append(swc.cases[:chosen], swc.cases[chosen+1:]...)
			swc.workCache = append(swc.workCache[:chosen], swc.workCache[chosen+1:]...)
			remaining = len(swc.cases)
			swc.mutex.Unlock()
			continue
		}

		prometheusMetric := value.Interface().(prometheus.Metric)
		pMetric := dto.Metric{}
		prometheusMetric.Write(&pMetric)
		tsMs := pMetric.GetTimestampMs()
		dims := make(map[string]string, len(pMetric.GetLabel()))
		for _, l := range pMetric.GetLabel() {
			key := l.GetName()
			value := l.GetValue()
			if key != "" && value != "" {
				dims[key] = value
			}
		}
		dims["cluster"] = swc.cfg.ClusterName

		swc.fillNodeDims(chosen, dims)

		podName, ok := dims["kubernetes_pod_name"]
		if ok {
			swc.mutex.Lock()
			serviceName, ok := swc.podToServiceMap[podName]
			swc.mutex.Unlock()
			if ok {
				dims["service"] = serviceName
			}
		}

		metricName := prometheusMetric.Desc().MetricName()
		timestamp := time.Unix(0, tsMs*time.Millisecond.Nanoseconds())

		for _, conv := range scrapper.ConvertMeric(&pMetric) {
			dp := datapoint.New(metricName+conv.MetricNameSuffix, scrapper.AppendDims(dims, conv.ExtraDims), conv.Value, conv.MType, timestamp)
			log.WithField("name", metricName+conv.MetricNameSuffix).Debug("adding datapoint")
			ret[i] = dp
			i++
			if i == maxDatapoints {
				sort.Sort(sortableDatapoint(ret))
				log.WithField("num", maxDatapoints).Info("forwarding datapoints")
				if err := swc.forwarder.AddDatapoints(ctx, ret); err != nil {
					log.WithError(err).Error("failed to forward datapoints")
				}
				i = 0
			}
		}
	}
}
