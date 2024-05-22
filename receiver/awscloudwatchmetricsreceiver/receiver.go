// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package awscloudwatchmetricsreceiver // import "github.com/open-telemetry/opentelemetry-collector-contrib/receiver/awscloudwatchmetricsreceiver"

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strconv"
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

// CloudWatchAPI is an interface to represent subset of AWS CloudWatch metrics functionality.
type client interface {
	GetMetricData(ctx context.Context, params *cloudwatch.GetMetricDataInput, optFns ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error)
	ListMetrics(ctx context.Context, params *cloudwatch.ListMetricsInput, optFns ...func(*cloudwatch.Options)) (*cloudwatch.ListMetricsOutput, error)
}

func buildGetMetricDataQueries(metric *request, id int) types.MetricDataQuery {
	return types.MetricDataQuery{
		Id:         aws.String(fmt.Sprintf("m_%d", rand.Int())),
		ReturnData: aws.Bool(true),
		Label:      aws.String(fmt.Sprintf("%d", id)),
		MetricStat: &types.MetricStat{
			Metric: &types.Metric{
				Namespace:  aws.String(metric.Namespace),
				MetricName: aws.String(metric.MetricName),
				Dimensions: metric.Dimensions,
			},
			Period: aws.Int32(int32(metric.Period / time.Second)),
			Stat:   aws.String(metric.AwsAggregation),
			//Unit:   FetchStandardUnit(metric.Namespace, metric.MetricName),
		},
	}
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
				append(metricDataInput[idx].MetricDataQueries, buildGetMetricDataQueries(&chunk[ydx], (idx*maxNumberOfElements)+ydx))
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

				requests = append(requests, request{
					Namespace:      group.Namespace,
					MetricName:     namedConfig.MetricName,
					Period:         group.Period,
					AwsAggregation: namedConfig.AwsAggregation,
					Dimensions:     dimensions,
				})
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

	if err := m.configureAWSClient(ctx); err != nil {
		m.logger.Error("unable to establish connection to cloudwatch", zap.Error(err))
		return
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
			if err := m.poll(ctx); err != nil {
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

// pollForMetrics: Without paginator functionality
/*func (m *metricReceiver) pollForMetricsBackup(ctx context.Context, startTime time.Time, endTime time.Time) error {
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
			metrics := m.parseMetrics(ctx, observedTime, m.requests, output)
			if metrics.MetricCount() > 0 {
				if err := m.consumer.ConsumeMetrics(ctx, metrics); err != nil {
					m.logger.Error("unable to consume metrics", zap.Error(err))
				}
			}
		}
	}
	return nil
}*/

func (m *metricReceiver) pollForMetrics(ctx context.Context, startTime time.Time, endTime time.Time) error {
	select {
	case _, ok := <-m.doneChan:
		if !ok {
			return nil
		}
	default:
		filters := m.request(startTime, endTime)
		for _, filter := range filters {
			// Step2: Work similar to GetMetricData()
			paginator := cloudwatch.NewGetMetricDataPaginator(m.client, &filter)
			for paginator.HasMorePages() {
				output, err := paginator.NextPage(ctx)
				if err != nil {
					m.logger.Error("unable to retrieve metric data from cloudwatch", zap.Error(err))
					continue
				}
				observedTime := pcommon.NewTimestampFromTime(time.Now())
				metrics := m.parseMetrics(ctx, observedTime, m.requests, output)
				if metrics.MetricCount() > 0 {
					if err := m.consumer.ConsumeMetrics(ctx, metrics); err != nil {
						m.logger.Error("unable to consume metrics", zap.Error(err))
						break
					}
				}
			}
		}
	}
	return nil
}

func (m *metricReceiver) getAWSAccessKeyID(ctx context.Context) (string, error) {
	// Load AWS SDK configuration from default sources
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return "", err
	}

	// Access AWS credentials
	creds, err := cfg.Credentials.Retrieve(ctx)
	if err != nil {
		return "", err
	}

	return creds.AccessKeyID, nil
}

func convertValueAndUnit(value float64, standardUnit types.StandardUnit, otelUnit string) (float64, string) {
	switch standardUnit {
	case StandardUnitMinutes:
		// Convert from Minutes to Seconds
		value *= 60
		otelUnit = "s"
	case StandardUnitGibibytes:
		// Convert from Gibibytes to Gigabytes
		value *= 1.073741824 // Conversion factor: 1024^3 / 1000^3
		otelUnit = "GBy"
	case StandardUnitMebibytes:
		// Convert from Mebibytes to Megabytes
		value *= 1.048576 // Conversion factor: 1024^2 / 1000^2
		otelUnit = "MBy"
	}
	return value, otelUnit
}

func (m *metricReceiver) parseMetrics(ctx context.Context, nowts pcommon.Timestamp, nr []request, resp *cloudwatch.GetMetricDataOutput) pmetric.Metrics {
	pdm := pmetric.NewMetrics()
	rms := pdm.ResourceMetrics()
	rm := rms.AppendEmpty()

	resourceAttrs := rm.Resource().Attributes()
	resourceAttrs.PutStr(conventions.AttributeCloudProvider, conventions.AttributeCloudProviderAWS)
	resourceAttrs.PutStr(conventions.AttributeCloudRegion, m.region)
	resourceAttrs.PutStr("channel", conventions.AttributeCloudProviderAWS)

	// Temporary for now, until we find cloud.account.id
	accessKeyID, err := m.getAWSAccessKeyID(ctx)
	if err != nil {
		m.logger.Error("Error retrieving AWS credentials:", zap.Error(err))
		return pdm
	}
	resourceAttrs.PutStr("cloud.account.id", accessKeyID)

	ilms := rm.ScopeMetrics()
	ilm := ilms.AppendEmpty()
	ms := ilm.Metrics()
	ms.EnsureCapacity(len(m.requests))
	//atts := make(map[string]interface{})

	for idx, results := range resp.MetricDataResults {

		reqIndex, err := strconv.Atoi(*results.Label)
		if err != nil {
			m.logger.Debug("illegal metric label", zap.Error(err))
			continue
		}

		if len(results.Timestamps) == 0 {
			now := time.Now()
			results.Timestamps = append(results.Timestamps, now)
			results.Values = append(results.Values, 0)
		}

		req := nr[reqIndex]
		standardUnit := FetchStandardUnit(req.Namespace, req.MetricName)
		otelUnit := FetchOtelUnit(standardUnit)

		mdp := ms.AppendEmpty()
		mdp.SetName(fmt.Sprintf("%s.%s", req.Namespace, req.MetricName))
		mdp.SetDescription(fmt.Sprintf("CloudWatch metric %s", req.MetricName))
		dps := mdp.SetEmptyGauge().DataPoints()

		// number of values *always* equals number of timestamps
		for point := range results.Values {
			ts, value := results.Timestamps[point], results.Values[point]

			// Convert value and unit if necessary
			value, otelUnit = convertValueAndUnit(value, standardUnit, otelUnit)

			dp := dps.AppendEmpty()
			dp.SetTimestamp(nowts)
			dp.SetStartTimestamp(pcommon.NewTimestampFromTime(ts))
			dp.SetDoubleValue(value)

			for _, dim := range nr[idx].Dimensions {
				dp.Attributes().PutStr(*dim.Name, *dim.Value)
			}

			dp.Attributes().PutStr("Namespace", req.Namespace)
			dp.Attributes().PutStr("MetricName", req.MetricName)
			dp.Attributes().PutStr("AWSUnit", string(standardUnit))
			dp.Attributes().PutStr("OTELUnit", otelUnit)
		}
		mdp.SetUnit(otelUnit)
	}
	return pdm
}

// autoDiscoverRequests: Without paginator functionality
/*func (m *metricReceiver) autoDiscoverRequestsBackup(ctx context.Context, auto *AutoDiscoverConfig) ([]request, error) {
	m.logger.Debug("discovering metrics", zap.String("namespace", auto.Namespace))

	var requests []request
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
				m.logger.Debug("reached limit of number of metrics, try increasing the limit config to increase the number of individual metrics polled")
				break
			}
			requests = append(requests, request{
				Namespace:      *metric.Namespace,
				MetricName:     *metric.MetricName,
				Dimensions:     metric.Dimensions,
				Period:         auto.Period,
				AwsAggregation: auto.AwsAggregation,
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
}*/

func (m *metricReceiver) autoDiscoverRequests(ctx context.Context, auto *AutoDiscoverConfig) ([]request, error) {
	m.logger.Debug("discovering metrics", zap.String("namespace", auto.Namespace))

	var requests []request
	// Step1: Work similar to ListMetrics()
	paginator := cloudwatch.NewListMetricsPaginator(m.client, &cloudwatch.ListMetricsInput{
		Namespace:      aws.String(auto.Namespace),
		RecentlyActive: "PT3H",
	})
	for paginator.HasMorePages() {
		if len(requests) > auto.Limit {
			m.logger.Debug(auto.Namespace + ": reached limit of number of metrics, try increasing the limit config to increase the number of individual metrics polled")
		}
		out, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, metric := range out.Metrics {
			requests = append(requests, request{
				Namespace:      *metric.Namespace,
				MetricName:     *metric.MetricName,
				Period:         auto.Period,
				AwsAggregation: auto.AwsAggregation,
				Dimensions:     metric.Dimensions,
			})
		}
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
		config.WithRegion(m.region),
		config.WithSharedConfigProfile(m.profile),
		config.WithEC2IMDSEndpoint(m.imdsEndpoint),
	)
	if err != nil {
		return err
	}
	m.client = cloudwatch.NewFromConfig(cfg)
	return nil
}
