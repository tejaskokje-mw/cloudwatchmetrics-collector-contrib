// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package awscloudwatchmetricsreceiver // import "github.com/open-telemetry/opentelemetry-collector-contrib/receiver/awscloudwatchmetricsreceiver"

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	conventions "go.opentelemetry.io/collector/semconv/v1.6.1"
	"go.uber.org/zap"
)

const (
	maxNumberOfElements = 500
)

type metricReceiver struct {
	region        string
	profile       string
	imdsEndpoint  string
	pollInterval  time.Duration
	nextStartTime time.Time
	logger        *zap.Logger
	client        client
	autoDiscover  *AutoDiscoverConfig
	requests      []request
	consumer      consumer.Metrics
	wg            *sync.WaitGroup
	doneChan      chan bool
}

type request struct {
	Namespace      string
	MetricName     string
	Period         time.Duration
	AwsAggregation string
	Dimensions     []types.Dimension
}

var EC2MetricToUnit = map[string]types.StandardUnit{
	"CPUUtilization":                types.StandardUnitPercent,
	"DiskReadOps":                   types.StandardUnitCount,
	"DiskWriteOps":                  types.StandardUnitCount,
	"DiskReadBytes":                 types.StandardUnitBytes,
	"DiskWriteBytes":                types.StandardUnitBytes,
	"NetworkIn":                     types.StandardUnitBytes,
	"NetworkOut":                    types.StandardUnitBytes,
	"NetworkPacketsIn":              types.StandardUnitCount,
	"NetworkPacketsOut":             types.StandardUnitCount,
	"CPUCreditUsage":                types.StandardUnitCount,
	"CPUCreditBalance":              types.StandardUnitCount,
	"CPUSurplusCreditBalance":       types.StandardUnitCount,
	"CPUSurplusCreditsCharged":      types.StandardUnitCount,
	"DedicatedHOstCPUUtilization":   types.StandardUnitPercent,
	"EBSReadOps":                    types.StandardUnitCount,
	"EBSWriteOps":                   types.StandardUnitCount,
	"EBSReadBytes":                  types.StandardUnitBytes,
	"EBSWriteBytes":                 types.StandardUnitBytes,
	"MetadataNoToken":               types.StandardUnitCount,
	"EBSIOBalance%":                 types.StandardUnitPercent,
	"EBSByteBalance%":               types.StandardUnitBytes,
	"StatusCheckFailed":             types.StandardUnitCount,
	"StatusCheckFailed_Instance":    types.StandardUnitCount,
	"StatusCheckFailed_System":      types.StandardUnitCount,
	"StatusCheckFailed_AttachedEBS": types.StandardUnitCount,
	"VolumeStalledIOCheck":          types.StandardUnitCount,
	"MemoryUtilization":             types.StandardUnitPercent,
}

// CloudWatchAPI is an interface to represent subset of AWS CloudWatch metrics functionality.
type client interface {
	GetMetricData(ctx context.Context, params *cloudwatch.GetMetricDataInput, optFns ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error)
	ListMetrics(ctx context.Context, params *cloudwatch.ListMetricsInput, optFns ...func(*cloudwatch.Options)) (*cloudwatch.ListMetricsOutput, error)
}

func buildGetMetricDataQueries(metric *request) types.MetricDataQuery {
	mdq := types.MetricDataQuery{
		Id:         aws.String(fmt.Sprintf("m_%d", rand.Int())),
		ReturnData: aws.Bool(true),
	}
	mdq.MetricStat = &types.MetricStat{
		Metric: &types.Metric{
			Namespace:  aws.String(metric.Namespace),
			MetricName: aws.String(metric.MetricName),
			Dimensions: metric.Dimensions,
		},
		Period: aws.Int32(int32(metric.Period / time.Second)),
		Stat:   aws.String(metric.AwsAggregation),
		Unit:   EC2MetricToUnit[metric.MetricName],
	}
	return mdq
}

func chunkSlice(requests []request, maxSize int) [][]request {
	var slicedMetrics [][]request
	for i := 0; i < len(requests); i += maxSize {
		end := i + maxSize

		if end > len(requests) {
			end = len(requests)
		}
		slicedMetrics = append(slicedMetrics, requests[i:end])
	}
	return slicedMetrics
}

// divide up into slices of 500, then execute
// Split requests slices into small slices no longer than 500 elements
// GetMetricData only allows 500 elements in a slice, otherwise we'll get validation error
// Avoids making a network call for each metric configured
func (m *metricReceiver) request(st, et time.Time) []cloudwatch.GetMetricDataInput {

	chunks := chunkSlice(m.requests, maxNumberOfElements)
	metricDataInput := make([]cloudwatch.GetMetricDataInput, len(chunks))

	for idx, chunk := range chunks {
		for ydx := range chunk {
			metricDataInput[idx].StartTime, metricDataInput[idx].EndTime = aws.Time(st), aws.Time(et)
			metricDataInput[idx].MetricDataQueries =
				append(metricDataInput[idx].MetricDataQueries, buildGetMetricDataQueries(&chunk[ydx]))
		}
	}
	return metricDataInput
}

func newMetricReceiver(cfg *Config, logger *zap.Logger, consumer consumer.Metrics) *metricReceiver {
	var requests []request

	if cfg.Metrics.Group != nil {
		for _, group := range cfg.Metrics.Group {
			for _, namedConfig := range group.MetricName {
				var dimensions []types.Dimension

				for _, dimConfig := range namedConfig.Dimensions {
					dimensions = append(dimensions, types.Dimension{
						Name:  aws.String(dimConfig.Name),
						Value: aws.String(dimConfig.Value),
					})
				}

				namedRequest := request{
					Namespace:      group.Namespace,
					MetricName:     namedConfig.MetricName,
					Period:         group.Period,
					AwsAggregation: namedConfig.AwsAggregation,
					Dimensions:     dimensions,
				}

				requests = append(requests, namedRequest)
			}
		}
	}

	return &metricReceiver{
		region:        cfg.Region,
		profile:       cfg.Profile,
		imdsEndpoint:  cfg.IMDSEndpoint,
		pollInterval:  cfg.PollInterval,
		nextStartTime: time.Now().Add(-cfg.PollInterval),
		logger:        logger,
		autoDiscover:  cfg.Metrics.AutoDiscover,
		wg:            &sync.WaitGroup{},
		consumer:      consumer,
		requests:      requests,
		doneChan:      make(chan bool),
	}
}

func (m *metricReceiver) Start(ctx context.Context, _ component.Host) error {
	m.logger.Debug("starting to poll for CloudWatch metrics")
	m.wg.Add(1)
	go m.startPolling(ctx)
	return nil
}

func (m *metricReceiver) Shutdown(_ context.Context) error {
	m.logger.Debug("shutting down awscloudwatchmetrics receiver")
	close(m.doneChan)
	m.wg.Wait()
	return nil
}

func (m *metricReceiver) startPolling(ctx context.Context) {
	defer m.wg.Done()

	err := m.configureAWSClient(ctx)
	if err != nil {
		m.logger.Error("unable to establish connection to cloudwatch", zap.Error(err))
	}

	t := time.NewTicker(m.pollInterval)

	for {
		select {
		case <-ctx.Done():
			return
		case <-m.doneChan:
			return
		case <-t.C:
			if m.autoDiscover != nil {
				requests, err := m.autoDiscoverRequests(ctx, m.autoDiscover)
				if err != nil {
					m.logger.Debug("couldn't discover metrics", zap.Error(err))
					continue
				}
				m.requests = requests

			}
			err := m.poll(ctx)
			if err != nil {
				m.logger.Error("there was an error during polling", zap.Error(err))
			}
		}
	}
}

func (m *metricReceiver) poll(ctx context.Context) error {
	var errs error
	startTime := m.nextStartTime
	endTime := time.Now()
	if err := m.pollForMetrics(ctx, startTime, endTime); err != nil {
		errs = errors.Join(errs, err)
	}
	m.nextStartTime = endTime
	return errs
}

func (m *metricReceiver) pollForMetrics(ctx context.Context, startTime time.Time, endTime time.Time) error {
	select {
	case _, ok := <-m.doneChan:
		if !ok {
			return nil
		}
	default:
		filters := m.request(startTime, endTime)
		nextToken := aws.String("")
		for _, filter := range filters {
			if *nextToken != "" {
				filter.NextToken = nextToken
			}
			output, err := m.client.GetMetricData(ctx, &filter)
			nextToken = output.NextToken
			if err != nil {
				m.logger.Error("unable to retrieve metric data from cloudwatch", zap.Error(err))
				continue
			}
			observedTime := pcommon.NewTimestampFromTime(time.Now())
			metrics := m.parseMetrics(observedTime, m.requests, output)
			if metrics.MetricCount() > 0 {
				if err := m.consumer.ConsumeMetrics(ctx, metrics); err != nil {
					m.logger.Error("unable to consume metrics", zap.Error(err))
				}
			}
		}
	}
	return nil
}

func (m *metricReceiver) parseMetrics(nowts pcommon.Timestamp, nr []request, resp *cloudwatch.GetMetricDataOutput) pmetric.Metrics {
	pdm := pmetric.NewMetrics()
	rms := pdm.ResourceMetrics()

	rm := rms.AppendEmpty()

	resourceAttrs := rm.Resource().Attributes()
	resourceAttrs.PutStr(conventions.AttributeCloudProvider, conventions.AttributeCloudProviderAWS)
	resourceAttrs.PutStr(conventions.AttributeCloudRegion, m.region)

	ilms := rm.ScopeMetrics()
	ilm := ilms.AppendEmpty()
	ms := ilm.Metrics()
	ms.EnsureCapacity(len(m.requests))
	//atts := make(map[string]interface{})
	for idx, results := range resp.MetricDataResults {
		if len(results.Timestamps) == 0 {
			m.logger.Debug("no data points found for metric", zap.String("metric", nr[idx].MetricName))
			continue
		}
		mdp := ms.AppendEmpty()
		mdp.SetName(fmt.Sprintf("%s.%s", nr[idx].Namespace, nr[idx].MetricName))
		mdp.SetDescription(fmt.Sprintf("CloudWatch metric %s", nr[idx].MetricName))
		unit := EC2MetricToUnit[nr[idx].MetricName]
		mdp.SetUnit(string(unit))
		var dps pmetric.NumberDataPointSlice
		switch unit {
		case types.StandardUnitCount:
			fallthrough
		case types.StandardUnitBytes:
			sum := mdp.SetEmptySum()
			sum.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
			sum.SetIsMonotonic(true)
			dps = sum.DataPoints()
		case types.StandardUnitPercent:
			//mdp.SetAggregationTemporality(pmetric.AggregationTemporalityDelta)
			gauge := mdp.SetEmptyGauge()
			gauge.DataPoints()
			dps = gauge.DataPoints()
		default:
			m.logger.Debug("unsupported unit", zap.String("unit", string(unit)),
				zap.String("metric", nr[idx].MetricName))
			continue
		}

		/*for _, dim := range nr.Dimensions {
			atts[*dim.Name] = dim.Value
		}*/

		// number of values *always* equals number of timestamps
		for point := range results.Values {
			ts, value := results.Timestamps[point], results.Values[point]
			dp := dps.AppendEmpty()
			dp.SetTimestamp(nowts)
			dp.SetStartTimestamp(pcommon.NewTimestampFromTime(ts))
			dp.SetDoubleValue(value)
			for _, dim := range nr[idx].Dimensions {
				dp.Attributes().PutStr(*dim.Name, *dim.Value)
			}
		}
	}
	return pdm
}

func (m *metricReceiver) autoDiscoverRequests(ctx context.Context, auto *AutoDiscoverConfig) ([]request, error) {
	m.logger.Debug("discovering metrics", zap.String("namespace", auto.Namespace))

	requests := []request{}
	input := &cloudwatch.ListMetricsInput{
		Namespace: aws.String(auto.Namespace),
	}

	nextToken := aws.String("")
	for {
		if *nextToken != "" {
			input.NextToken = nextToken
		}
		out, err := m.client.ListMetrics(ctx, input)
		if err != nil {
			return nil, err
		}

		for _, metric := range out.Metrics {
			if len(requests) > auto.Limit {
				m.logger.Debug("reached limit of number of metrics, try increasing the limit config to increase the number of individial metrics polled")
				break
			}
			requests = append(requests, request{Namespace: *metric.Namespace,
				MetricName: *metric.MetricName, Period: auto.Period,
				AwsAggregation: auto.AwsAggregation, Dimensions: metric.Dimensions,
			})
		}

		// Manual Pagination: Check if more data is available.
		if out.NextToken == nil {
			break
		}
		input.NextToken = out.NextToken
	}

	m.logger.Debug("number of metrics discovered", zap.Int("metrics", len(requests)))
	return requests, nil
}

func (m *metricReceiver) configureAWSClient(ctx context.Context) error {
	if m.client != nil {
		return nil
	}

	// if "", helper functions (withXXX) ignores parameter
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(m.region), config.WithEC2IMDSEndpoint(m.imdsEndpoint), config.WithSharedConfigProfile(m.profile))
	m.client = cloudwatch.NewFromConfig(cfg)
	return err
}
