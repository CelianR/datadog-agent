// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2022-present Datadog, Inc.

package serializerexporter

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/exporter/exportertest"
	"go.opentelemetry.io/collector/pdata/pmetric"

	"github.com/DataDog/datadog-agent/pkg/metrics"
	"github.com/DataDog/datadog-agent/pkg/serializer"
	"github.com/DataDog/datadog-agent/pkg/tagset"
)

var _ serializer.MetricSerializer = (*metricRecorder)(nil)

type metricRecorder struct {
	serializer.Serializer // embed for implementing serializer.MetricSerializer

	sketchSeriesList metrics.SketchSeriesList
	series           []*metrics.Serie
}

func (r *metricRecorder) SendSketch(s metrics.SketchesSource) error {
	for s.MoveNext() {
		c := s.Current()
		if c == nil {
			continue
		}
		r.sketchSeriesList = append(r.sketchSeriesList, c)
	}
	return nil
}

func (r *metricRecorder) SendIterableSeries(s metrics.SerieSource) error {
	for s.MoveNext() {
		c := s.Current()
		if c == nil {
			continue
		}
		r.series = append(r.series, c)
	}
	return nil
}

func Test_ConsumeMetrics_Tags(t *testing.T) {
	const (
		histogramMetricName        = "test.histogram"
		numberMetricName           = "test.gauge"
		histogramRuntimeMetricName = "process.runtime.dotnet.exceptions.count"
		numberRuntimeMetricName    = "process.runtime.go.goroutines"
	)
	tests := []struct {
		name           string
		genMetrics     func(t *testing.T) pmetric.Metrics
		wantSketchTags tagset.CompositeTags
		wantSerieTags  tagset.CompositeTags
		extraTags      []string
	}{
		{
			name: "no tags",
			genMetrics: func(t *testing.T) pmetric.Metrics {
				h := pmetric.NewHistogramDataPoint()
				h.BucketCounts().FromRaw([]uint64{100})
				h.SetCount(100)
				h.SetSum(0)

				n := pmetric.NewNumberDataPoint()
				n.SetIntValue(777)
				return newMetrics(histogramMetricName, h, numberMetricName, n)
			},
			extraTags:      []string{},
			wantSketchTags: tagset.NewCompositeTags([]string{}, nil),
			wantSerieTags:  tagset.NewCompositeTags([]string{}, nil),
		},
		{
			name: "metric tags and extra tags",
			genMetrics: func(t *testing.T) pmetric.Metrics {
				h := pmetric.NewHistogramDataPoint()
				h.BucketCounts().FromRaw([]uint64{100})
				h.SetCount(100)
				h.SetSum(0)
				hAttrs := h.Attributes()
				hAttrs.PutStr("histogram_1_id", "value1")
				hAttrs.PutStr("histogram_2_id", "value2")
				hAttrs.PutStr("histogram_3_id", "value3")

				n := pmetric.NewNumberDataPoint()
				n.SetIntValue(777)
				nAttrs := n.Attributes()
				nAttrs.PutStr("gauge_1_id", "value1")
				nAttrs.PutStr("gauge_2_id", "value2")
				nAttrs.PutStr("gauge_3_id", "value3")
				return newMetrics(histogramMetricName, h, numberMetricName, n)
			},
			extraTags: []string{"serverless_tag1:test1", "serverless_tag2:test2", "serverless_tag3:test3"},
			wantSketchTags: tagset.NewCompositeTags(
				[]string{
					"serverless_tag1:test1", "serverless_tag2:test2", "serverless_tag3:test3",
					"histogram_1_id:value1", "histogram_2_id:value2", "histogram_3_id:value3",
				},
				nil,
			),
			wantSerieTags: tagset.NewCompositeTags(
				[]string{
					"serverless_tag1:test1", "serverless_tag2:test2", "serverless_tag3:test3",
					"gauge_1_id:value1", "gauge_2_id:value2", "gauge_3_id:value3",
				},
				nil,
			),
		},
		{
			name: "runtime metrics, no tags",
			genMetrics: func(t *testing.T) pmetric.Metrics {
				h := pmetric.NewHistogramDataPoint()
				h.BucketCounts().FromRaw([]uint64{100})
				h.SetCount(100)
				h.SetSum(0)

				n := pmetric.NewNumberDataPoint()
				n.SetIntValue(777)
				return newMetrics(histogramMetricName, h, numberMetricName, n)
			},
			wantSketchTags: tagset.NewCompositeTags([]string{}, nil),
			wantSerieTags:  tagset.NewCompositeTags([]string{}, nil),
		},
		{
			name: "runtime metrics, metric tags and extra tags",
			genMetrics: func(t *testing.T) pmetric.Metrics {
				h := pmetric.NewHistogramDataPoint()
				h.BucketCounts().FromRaw([]uint64{100})
				h.SetCount(100)
				h.SetSum(0)
				hAttrs := h.Attributes()
				hAttrs.PutStr("histogram_1_id", "value1")
				hAttrs.PutStr("histogram_2_id", "value2")
				hAttrs.PutStr("histogram_3_id", "value3")

				n := pmetric.NewNumberDataPoint()
				n.SetIntValue(777)
				nAttrs := n.Attributes()
				nAttrs.PutStr("gauge_1_id", "value1")
				nAttrs.PutStr("gauge_2_id", "value2")
				nAttrs.PutStr("gauge_3_id", "value3")
				return newMetrics(histogramRuntimeMetricName, h, numberRuntimeMetricName, n)
			},
			extraTags: []string{"serverless_tag1:test1", "serverless_tag2:test2", "serverless_tag3:test3"},
			wantSketchTags: tagset.NewCompositeTags(
				[]string{
					"serverless_tag1:test1", "serverless_tag2:test2", "serverless_tag3:test3",
					"histogram_1_id:value1", "histogram_2_id:value2", "histogram_3_id:value3",
				},
				nil,
			),
			wantSerieTags: tagset.NewCompositeTags(
				[]string{
					"serverless_tag1:test1", "serverless_tag2:test2", "serverless_tag3:test3",
					"gauge_1_id:value1", "gauge_2_id:value2", "gauge_3_id:value3",
				},
				nil,
			),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &metricRecorder{}
			ctx := context.Background()
			f := NewFactory(rec, &MockTagEnricher{}, func(context.Context) (string, error) {
				return "", nil
			})
			cfg := f.CreateDefaultConfig().(*exporterConfig)
			cfg.Metrics.Tags = strings.Join(tt.extraTags, ",")
			exp, err := f.CreateMetricsExporter(
				ctx,
				exportertest.NewNopCreateSettings(),
				cfg,
			)
			require.NoError(t, err)
			require.NoError(t, exp.Start(ctx, componenttest.NewNopHost()))
			require.NoError(t, exp.ConsumeMetrics(ctx, tt.genMetrics(t)))
			require.NoError(t, exp.Shutdown(ctx))

			if tt.wantSketchTags.Len() > 0 {
				assert.Equal(t, tt.wantSketchTags, rec.sketchSeriesList[0].Tags)
			} else {
				assert.Equal(t, tagset.NewCompositeTags([]string{}, nil), rec.sketchSeriesList[0].Tags)
			}
			assert.True(t, len(rec.series) > 0)
			for _, s := range rec.series {
				if s.Name == "datadog.agent.otlp.metrics" {
					assert.Equal(t, tagset.NewCompositeTags([]string{}, nil), s.Tags)
				}
				if s.Name == "datadog.agent.otlp.runtime_metrics" {
					assert.True(t, s.Tags.Find(func(tag string) bool {
						return tag == "language:go" || tag == "language:dotnet"
					}))
				}
				if s.Name == numberMetricName {
					if tt.wantSerieTags.Len() > 0 {
						assert.Equal(t, tt.wantSerieTags, s.Tags)
					} else {
						assert.Equal(t, tagset.NewCompositeTags([]string{}, nil), s.Tags)
					}
				}
			}
		})
	}
}

func newMetrics(
	histogramMetricName string,
	histogramDataPoint pmetric.HistogramDataPoint,
	numberMetricName string,
	numberDataPoint pmetric.NumberDataPoint,
) pmetric.Metrics {
	md := pmetric.NewMetrics()
	rms := md.ResourceMetrics()
	rm := rms.AppendEmpty()
	ilms := rm.ScopeMetrics()
	ilm := ilms.AppendEmpty()
	metricsArray := ilm.Metrics()
	metricsArray.AppendEmpty() // first one is TypeNone to test that it's ignored

	// Histgram
	met := metricsArray.AppendEmpty()
	met.SetName(histogramMetricName)
	met.SetEmptyHistogram()
	met.Histogram().SetAggregationTemporality(pmetric.AggregationTemporalityDelta)
	hdps := met.Histogram().DataPoints()
	hdp := hdps.AppendEmpty()
	hdp.SetCount(histogramDataPoint.Count())
	hdp.SetSum(histogramDataPoint.Sum())
	histogramDataPoint.BucketCounts().CopyTo(hdp.BucketCounts())
	histogramDataPoint.ExplicitBounds().CopyTo(hdp.ExplicitBounds())
	hdp.SetTimestamp(histogramDataPoint.Timestamp())
	hdpAttrs := hdp.Attributes()
	histogramDataPoint.Attributes().CopyTo(hdpAttrs)

	// Gauge
	met = metricsArray.AppendEmpty()
	met.SetName(numberMetricName)
	met.SetEmptyGauge()
	gdps := met.Gauge().DataPoints()
	gdp := gdps.AppendEmpty()
	gdp.SetTimestamp(numberDataPoint.Timestamp())
	gdp.SetIntValue(numberDataPoint.IntValue())
	gdpAttrs := gdp.Attributes()
	numberDataPoint.Attributes().CopyTo(gdpAttrs)

	return md
}
