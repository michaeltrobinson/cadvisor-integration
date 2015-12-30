package scraper

import (
	"fmt"

	dto "github.com/prometheus/client_model/go"
	"github.com/signalfx/golib/datapoint"
)

func appendDims(a map[string]string, b map[string]string) map[string]string {
	ret := make(map[string]string, len(a)+len(b))
	for k, v := range a {
		ret[k] = v
	}
	for k, v := range b {
		ret[k] = v
	}
	return ret
}

func convertMetric(m *dto.Metric) []metricConversion {
	if m.Counter != nil {
		return []metricConversion{
			{
				MType: datapoint.Counter,
				Value: datapoint.NewFloatValue(m.Counter.GetValue()),
			},
		}
	}
	if m.Gauge != nil {
		return []metricConversion{
			{
				MType: datapoint.Gauge,
				Value: datapoint.NewFloatValue(m.Gauge.GetValue()),
			},
		}
	}
	if m.Summary != nil {
		ret := []metricConversion{
			{
				MetricNameSuffix: "_sum",
				MType:            datapoint.Counter,
				Value:            datapoint.NewFloatValue(m.Summary.GetSampleSum()),
			},
			{
				MetricNameSuffix: "_count",
				MType:            datapoint.Counter,
				Value:            datapoint.NewIntValue(int64(m.Summary.GetSampleCount())),
			},
		}
		for _, q := range m.Summary.Quantile {
			ret = append(ret, metricConversion{
				MType: datapoint.Gauge,
				Value: datapoint.NewIntValue(int64(q.GetValue())),
				ExtraDims: map[string]string{
					"quantile": fmt.Sprintf("%.2f", q.GetQuantile()*100),
				},
			})
		}
		return ret
	}
	if m.Histogram != nil {
		ret := []metricConversion{
			{
				MetricNameSuffix: "_sum",
				MType:            datapoint.Counter,
				Value:            datapoint.NewFloatValue(m.Histogram.GetSampleSum()),
			},
			{
				MetricNameSuffix: "_count",
				MType:            datapoint.Counter,
				Value:            datapoint.NewIntValue(int64(m.Histogram.GetSampleCount())),
			},
		}
		for _, b := range m.Histogram.Bucket {
			ret = append(ret, metricConversion{
				MType:            datapoint.Counter,
				MetricNameSuffix: "_bucket",
				Value:            datapoint.NewIntValue(int64(b.GetCumulativeCount())),
				ExtraDims: map[string]string{
					"le": fmt.Sprintf("%.2f", b.GetUpperBound()),
				},
			})
		}
		return ret
	}
	if m.Untyped != nil {
		return []metricConversion{
			{
				MType: datapoint.Gauge,
				Value: datapoint.NewFloatValue(m.Untyped.GetValue()),
			},
		}
	}
	return []metricConversion{}
}

type metricConversion struct {
	MType            datapoint.MetricType
	Value            datapoint.Value
	MetricNameSuffix string
	ExtraDims        map[string]string
}
