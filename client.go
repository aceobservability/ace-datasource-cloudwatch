package cloudwatch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awscw "github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cloudwatchtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	awslogs "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cloudwatchlogstypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"

	"github.com/aceobservability/ace/backend/pkg/datasource"
)

// Type is the RegisterDatasource key Ace uses for this module.
const Type = "cloudwatch"

const (
	defaultCloudWatchNamespace = "AWS/EC2"
	cloudWatchPollInterval     = 500 * time.Millisecond
)

// Client implements the Ace CloudWatch metrics+logs datasource.
type Client struct {
	cfg        datasource.Config
	parsed     cloudWatchConfig
	httpClient *http.Client
}

type cloudWatchConfig struct {
	Region          string
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	MetricNamespace string
	LogGroups       []string
}

type cloudWatchMetricQuery struct {
	Namespace  string            `json:"namespace"`
	MetricName string            `json:"metric_name"`
	Dimensions map[string]string `json:"dimensions"`
	Stat       string            `json:"stat"`
	Period     int32             `json:"period"`
	Unit       string            `json:"unit"`
	Label      string            `json:"label"`
	Expression string            `json:"expression"`
}

type cloudWatchLogsQuery struct {
	QueryString   string   `json:"query"`
	LogGroup      string   `json:"log_group"`
	LogGroupNames []string `json:"log_group_names"`
	Limit         int32    `json:"limit"`
}

type timedMetricValue struct {
	timestamp time.Time
	value     float64
}

// New constructs a CloudWatch datasource client.
// httpClient is required so Ace can inject DatasourceClient (dial/redirect policy).
func New(cfg datasource.Config, httpClient *http.Client) (*Client, error) {
	if httpClient == nil {
		return nil, fmt.Errorf("http client is required")
	}

	parsed, err := parseCloudWatchConfig(cfg.AuthConfig)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(parsed.Region) == "" {
		return nil, fmt.Errorf("cloudwatch region is required (set auth_config.region)")
	}

	return &Client{cfg: cfg, parsed: parsed, httpClient: httpClient}, nil
}

// HTTPClient returns the injected HTTP client. Ace SSRF tests inspect policy wiring.
func (c *Client) HTTPClient() *http.Client {
	return c.httpClient
}

func (c *Client) Query(ctx context.Context, query string, start, end time.Time, step time.Duration, limit int) (*datasource.QueryResult, error) {
	return c.QueryWithSignal(ctx, query, "metrics", start, end, step, limit)
}

func (c *Client) QueryWithSignal(ctx context.Context, query, signal string, start, end time.Time, step time.Duration, limit int) (*datasource.QueryResult, error) {
	normalizedSignal := strings.ToLower(strings.TrimSpace(signal))
	if normalizedSignal == "" {
		normalizedSignal = "metrics"
	}

	switch normalizedSignal {
	case "metrics":
		return c.queryMetrics(ctx, query, start, end, step)
	case "logs":
		return c.queryLogs(ctx, query, start, end, limit)
	default:
		return nil, fmt.Errorf("cloudwatch only supports metrics or logs signals")
	}
}

func (c *Client) queryMetrics(ctx context.Context, query string, start, end time.Time, step time.Duration) (*datasource.QueryResult, error) {
	metricQuery, err := parseCloudWatchMetricQuery(query, c.parsed.MetricNamespace, step)
	if err != nil {
		return nil, err
	}

	awsCfg, err := c.awsConfig(ctx)
	if err != nil {
		return nil, err
	}

	client := awscw.NewFromConfig(awsCfg)
	queryInput := cloudwatchtypes.MetricDataQuery{
		Id:         aws.String("m1"),
		ReturnData: aws.Bool(true),
	}

	label := strings.TrimSpace(metricQuery.Label)
	if label != "" {
		queryInput.Label = aws.String(label)
	}

	if strings.TrimSpace(metricQuery.Expression) != "" {
		queryInput.Expression = aws.String(strings.TrimSpace(metricQuery.Expression))
	} else {
		dimensions := make([]cloudwatchtypes.Dimension, 0, len(metricQuery.Dimensions))
		dimensionKeys := make([]string, 0, len(metricQuery.Dimensions))
		for key := range metricQuery.Dimensions {
			dimensionKeys = append(dimensionKeys, key)
		}
		sort.Strings(dimensionKeys)
		for _, key := range dimensionKeys {
			dimensions = append(dimensions, cloudwatchtypes.Dimension{
				Name:  aws.String(key),
				Value: aws.String(metricQuery.Dimensions[key]),
			})
		}

		metricStat := &cloudwatchtypes.MetricStat{
			Metric: &cloudwatchtypes.Metric{
				Namespace:  aws.String(metricQuery.Namespace),
				MetricName: aws.String(metricQuery.MetricName),
				Dimensions: dimensions,
			},
			Period: aws.Int32(metricQuery.Period),
			Stat:   aws.String(metricQuery.Stat),
		}
		if strings.TrimSpace(metricQuery.Unit) != "" {
			metricStat.Unit = cloudwatchtypes.StandardUnit(strings.TrimSpace(metricQuery.Unit))
		}

		queryInput.MetricStat = metricStat
	}

	request := &awscw.GetMetricDataInput{
		MetricDataQueries: []cloudwatchtypes.MetricDataQuery{queryInput},
		StartTime:         aws.Time(start),
		EndTime:           aws.Time(end),
		ScanBy:            cloudwatchtypes.ScanByTimestampAscending,
	}

	response, err := client.GetMetricData(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("cloudwatch metrics query failed: %w", err)
	}

	labels := map[string]string{}
	if metricQuery.MetricName != "" {
		labels["__name__"] = metricQuery.MetricName
		labels["metric_name"] = metricQuery.MetricName
		labels["namespace"] = metricQuery.Namespace
		labels["stat"] = metricQuery.Stat
		for key, value := range metricQuery.Dimensions {
			labels[key] = value
		}
	} else {
		labels["__name__"] = labelOrFallback(aws.ToString(queryInput.Label), "expression")
	}

	dataPoints := make([]timedMetricValue, 0)
	for _, result := range response.MetricDataResults {
		for idx, timestamp := range result.Timestamps {
			if idx >= len(result.Values) {
				continue
			}
			dataPoints = append(dataPoints, timedMetricValue{
				timestamp: timestamp,
				value:     result.Values[idx],
			})
		}
	}
	sort.Slice(dataPoints, func(i, j int) bool {
		return dataPoints[i].timestamp.Before(dataPoints[j].timestamp)
	})

	values := make([][]interface{}, 0, len(dataPoints))
	for _, point := range dataPoints {
		values = append(values, []interface{}{
			float64(point.timestamp.Unix()),
			strconv.FormatFloat(point.value, 'f', -1, 64),
		})
	}

	return &datasource.QueryResult{
		Status:     "success",
		ResultType: "metrics",
		Data: &datasource.QueryData{
			ResultType: "matrix",
			Result: []datasource.MetricResult{
				{
					Metric: labels,
					Values: values,
				},
			},
		},
	}, nil
}

func (c *Client) queryLogs(ctx context.Context, query string, start, end time.Time, limit int) (*datasource.QueryResult, error) {
	logsQuery, err := parseCloudWatchLogsQuery(query, c.parsed.LogGroups, limit)
	if err != nil {
		return nil, err
	}

	awsCfg, err := c.awsConfig(ctx)
	if err != nil {
		return nil, err
	}

	client := awslogs.NewFromConfig(awsCfg)
	startInput := &awslogs.StartQueryInput{
		StartTime:   aws.Int64(start.Unix()),
		EndTime:     aws.Int64(end.Unix()),
		QueryString: aws.String(logsQuery.QueryString),
		Limit:       aws.Int32(logsQuery.Limit),
	}

	if len(logsQuery.LogGroupNames) == 1 {
		startInput.LogGroupName = aws.String(logsQuery.LogGroupNames[0])
	} else {
		startInput.LogGroupNames = logsQuery.LogGroupNames
	}

	startResult, err := client.StartQuery(ctx, startInput)
	if err != nil {
		return nil, fmt.Errorf("cloudwatch logs query failed: %w", err)
	}
	queryID := aws.ToString(startResult.QueryId)
	if queryID == "" {
		return nil, fmt.Errorf("cloudwatch logs query did not return a query id")
	}

	ticker := time.NewTicker(cloudWatchPollInterval)
	defer ticker.Stop()

	for {
		output, err := client.GetQueryResults(ctx, &awslogs.GetQueryResultsInput{
			QueryId: aws.String(queryID),
		})
		if err != nil {
			return nil, fmt.Errorf("failed to fetch cloudwatch logs query results: %w", err)
		}

		switch output.Status {
		case cloudwatchlogstypes.QueryStatusComplete:
			return &datasource.QueryResult{
				Status:     "success",
				ResultType: "logs",
				Data: &datasource.QueryData{
					ResultType: "streams",
					Logs:       parseCloudWatchLogResults(output.Results),
				},
			}, nil
		case cloudwatchlogstypes.QueryStatusFailed:
			return nil, fmt.Errorf("cloudwatch logs query failed")
		case cloudwatchlogstypes.QueryStatusCancelled:
			return nil, fmt.Errorf("cloudwatch logs query was cancelled")
		case cloudwatchlogstypes.QueryStatusTimeout:
			return nil, fmt.Errorf("cloudwatch logs query timed out")
		case cloudwatchlogstypes.QueryStatusRunning, cloudwatchlogstypes.QueryStatusScheduled, cloudwatchlogstypes.QueryStatusUnknown:
			// Keep polling.
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (c *Client) awsConfig(ctx context.Context) (aws.Config, error) {
	loadOptions := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(c.parsed.Region),
		awsconfig.WithHTTPClient(c.httpClient),
	}

	endpoint := customEndpoint(c.cfg.URL)
	if endpoint != "" {
		loadOptions = append(loadOptions, awsconfig.WithBaseEndpoint(endpoint))
	}

	accessKeyID := strings.TrimSpace(c.parsed.AccessKeyID)
	secretAccessKey := strings.TrimSpace(c.parsed.SecretAccessKey)
	if accessKeyID != "" || secretAccessKey != "" {
		if accessKeyID == "" || secretAccessKey == "" {
			return aws.Config{}, fmt.Errorf("cloudwatch auth_config requires both access_key_id and secret_access_key")
		}
		loadOptions = append(loadOptions, awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			accessKeyID,
			secretAccessKey,
			c.parsed.SessionToken,
		)))
	} else if endpoint != "" {
		// Custom BaseEndpoint + default chain would SigV4-sign attacker or
		// RFC1918 URLs with platform credentials (confused deputy).
		return aws.Config{}, fmt.Errorf("cloudwatch custom endpoint requires access_key_id and secret_access_key")
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx, loadOptions...)
	if err != nil {
		return aws.Config{}, fmt.Errorf("failed to load aws config: %w", err)
	}
	return cfg, nil
}

func customEndpoint(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || strings.TrimSpace(parsed.Host) == "" {
		return ""
	}
	host := strings.ToLower(parsed.Hostname())
	if strings.HasSuffix(host, ".amazonaws.com") || strings.HasSuffix(host, ".amazon.com") {
		return ""
	}
	return strings.TrimRight(trimmed, "/")
}
