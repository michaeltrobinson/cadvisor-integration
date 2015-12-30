package scraper

import (
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/google/cadvisor/client"
	"github.com/google/cadvisor/metrics"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/signalfx/golib/datapoint"
	"github.com/signalfx/metricproxy/protocol/signalfx"
	"golang.org/x/net/context"

	kubeAPI "k8s.io/kubernetes/pkg/api"

	log "github.com/Sirupsen/logrus"
	dto "github.com/prometheus/client_model/go"
)

type work struct {
	serverURL  string
	collector  *metrics.PrometheusCollector
	chRecvOnly chan prometheus.Metric
}

func (s *work) DoWork(workRoutine int) {
	s.collector.Collect(s.chRecvOnly)
}

type workSet struct {
	forwarder     *signalfx.Forwarder
	maxDatapoints int
	clusterName   string

	workSet         []*work
	cases           []reflect.SelectCase
	podToServiceMap map[string]string
	hostIPtoNameMap map[string]kubeAPI.Node
	mutex           *sync.Mutex
}

func newWorkSet(forwarder *signalfx.Forwarder, maxDatapoints int, clusterName string) *workSet {
	return &workSet{
		workSet:       make([]*work, 0, 1),
		cases:         make([]reflect.SelectCase, 0, 1),
		forwarder:     forwarder,
		mutex:         &sync.Mutex{},
		maxDatapoints: maxDatapoints,
		clusterName:   clusterName,
	}
}

func (swc *workSet) addWork(work *work) {
	swc.mutex.Lock()
	defer swc.mutex.Unlock()

	swc.workSet = append(swc.workSet, work)
	c := reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(work.chRecvOnly)}
	swc.cases = append(swc.cases, c)
}

func (swc *workSet) buildWorkList(urlList []string) {
	for _, serverURL := range urlList {
		cadvisorClient, localERR := client.NewClient(serverURL)
		if localERR != nil {
			log.WithError(localERR).Error("failed connect to cadvisor server")
			continue
		}

		swc.addWork(&work{
			serverURL: serverURL,
			collector: metrics.NewPrometheusCollector(&cadvisorInfoProvider{
				cc: cadvisorClient,
			}, nameToLabel),
			chRecvOnly: make(chan prometheus.Metric),
		})
	}
}

func (swc *workSet) setPodToServiceMap(m map[string]string) {
	swc.mutex.Lock()
	defer swc.mutex.Unlock()

	swc.podToServiceMap = m
}

func (swc *workSet) setHostIPtoNameMap(m map[string]kubeAPI.Node) {
	swc.mutex.Lock()
	defer swc.mutex.Unlock()

	swc.hostIPtoNameMap = m
}

type eachWorkFunc func(int, *work) bool

func (swc *workSet) forEachWork(f eachWorkFunc) {
	swc.mutex.Lock()
	workSetCopy := make([]*work, len(swc.workSet))
	copy(workSetCopy, swc.workSet)
	swc.mutex.Unlock()

	for index, work := range workSetCopy {
		if !f(index, work) {
			return
		}
	}
}

func (swc *workSet) fillNodeDims(chosen int, dims map[string]string) {

	node, ok := func() (n kubeAPI.Node, b bool) {
		swc.mutex.Lock()
		defer func() {
			swc.mutex.Unlock()
			if r := recover(); r != nil {
				log.WithField("r", r).Info("recovered in fillNodeDims")
			}
		}()

		n, b = swc.hostIPtoNameMap[swc.workSet[chosen].serverURL]
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
func (swc *workSet) waitAndForward() {
	swc.mutex.Lock()
	remaining := len(swc.cases)
	swc.mutex.Unlock()

	ctx := context.Background()

	i := 0
	ret := make([]*datapoint.Datapoint, swc.maxDatapoints)
	for remaining > 0 {
		chosen, value, ok := reflect.Select(swc.cases)
		if !ok {
			// The chosen channel has been closed, so zero out the channel to disable the case
			swc.mutex.Lock()
			swc.cases[chosen].Chan = reflect.ValueOf(nil)
			swc.cases = append(swc.cases[:chosen], swc.cases[chosen+1:]...)
			swc.workSet = append(swc.workSet[:chosen], swc.workSet[chosen+1:]...)
			remaining = len(swc.cases)
			swc.mutex.Unlock()
			continue
		}

		prometheusMetric := value.Interface().(prometheus.Metric)
		pMetric := dto.Metric{}
		if err := prometheusMetric.Write(&pMetric); err != nil {
			log.WithError(err).Error("failed to write metric")
		}
		tsMs := pMetric.GetTimestampMs()
		dims := make(map[string]string, len(pMetric.GetLabel()))
		for _, l := range pMetric.GetLabel() {
			key := l.GetName()
			value := l.GetValue()
			if key != "" && value != "" {
				dims[key] = value
			}
		}
		dims["cluster"] = swc.clusterName

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

		for _, conv := range convertMetric(&pMetric) {
			dp := datapoint.New(metricName+conv.MetricNameSuffix, appendDims(dims, conv.ExtraDims), conv.Value, conv.MType, timestamp)
			log.WithField("name", metricName+conv.MetricNameSuffix).Debug("adding datapoint")
			ret[i] = dp
			i++
			if i == swc.maxDatapoints {
				sort.Sort(sortableDatapoint(ret))
				log.WithField("num", swc.maxDatapoints).Info("forwarding datapoints")
				if err := swc.forwarder.AddDatapoints(ctx, ret); err != nil {
					log.WithError(err).Error("failed to forward datapoints")
				}
				i = 0
			}
		}
	}
}
