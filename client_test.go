package cloudwatch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/smithy-go/encoding/cbor"

	"github.com/aceobservability/ace/backend/pkg/datasource"
)

func TestParseCloudWatchConfig(t *testing.T) {
	raw := json.RawMessage(`{
		"region": "us-east-1",
		"access_key_id": "AKIA123",
		"secret_access_key": "secret",
		"session_token": "token",
		"metric_namespace": "AWS/ApplicationELB",
		"log_group": "/aws/lambda/my-fn",
		"log_group_names": ["/aws/ecs/service-a", "/aws/ecs/service-b"]
	}`)

	cfg, err := parseCloudWatchConfig(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Region != "us-east-1" {
		t.Fatalf("expected region us-east-1, got %q", cfg.Region)
	}
	if cfg.AccessKeyID != "AKIA123" {
		t.Fatalf("expected access key id to round-trip")
	}
	if cfg.SecretAccessKey != "secret" {
		t.Fatalf("expected secret access key to round-trip")
	}
	if cfg.SessionToken != "token" {
		t.Fatalf("expected session token to round-trip")
	}
	if cfg.MetricNamespace != "AWS/ApplicationELB" {
		t.Fatalf("expected metric namespace to round-trip")
	}
	if len(cfg.LogGroups) != 3 {
		t.Fatalf("expected 3 log groups, got %d", len(cfg.LogGroups))
	}
}

func TestParseCloudWatchMetricQuery_JSON(t *testing.T) {
	query, err := parseCloudWatchMetricQuery(`{
		"namespace":"AWS/EC2",
		"metric_name":"CPUUtilization",
		"dimensions":{"InstanceId":"i-123"},
		"stat":"Average",
		"period":60
	}`, "", 15*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if query.Namespace != "AWS/EC2" {
		t.Fatalf("expected namespace AWS/EC2, got %q", query.Namespace)
	}
	if query.MetricName != "CPUUtilization" {
		t.Fatalf("expected metric name CPUUtilization, got %q", query.MetricName)
	}
	if query.Period != 60 {
		t.Fatalf("expected period 60, got %d", query.Period)
	}
	if query.Dimensions["InstanceId"] != "i-123" {
		t.Fatalf("expected dimension InstanceId=i-123")
	}
}

func TestParseCloudWatchMetricQuery_PlainString(t *testing.T) {
	query, err := parseCloudWatchMetricQuery("AWS/Lambda:Duration", "", 30*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if query.Namespace != "AWS/Lambda" {
		t.Fatalf("expected namespace AWS/Lambda, got %q", query.Namespace)
	}
	if query.MetricName != "Duration" {
		t.Fatalf("expected metric name Duration, got %q", query.MetricName)
	}
	if query.Period != 60 {
		t.Fatalf("expected period to be clamped to 60, got %d", query.Period)
	}
}

func TestParseCloudWatchLogsQuery_UsesDefaultLogGroup(t *testing.T) {
	query, err := parseCloudWatchLogsQuery("fields @timestamp, @message | limit 20", []string{"/aws/lambda/my-fn"}, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if query.QueryString == "" {
		t.Fatalf("expected query string")
	}
	if len(query.LogGroupNames) != 1 || query.LogGroupNames[0] != "/aws/lambda/my-fn" {
		t.Fatalf("expected default log group to be used, got %#v", query.LogGroupNames)
	}
	if query.Limit != 1000 {
		t.Fatalf("expected default limit 1000, got %d", query.Limit)
	}
}

func TestParseCloudWatchLogsQuery_RequiresLogGroup(t *testing.T) {
	_, err := parseCloudWatchLogsQuery("fields @timestamp", nil, 100)
	if err == nil {
		t.Fatalf("expected error when log groups are missing")
	}
}

func TestNew_requiresHTTPClient(t *testing.T) {
	t.Parallel()

	client, err := New(datasource.Config{
		Type:       Type,
		AuthConfig: json.RawMessage(`{"region":"us-east-1"}`),
	}, nil)
	if err == nil {
		t.Fatal("expected error for nil http client")
	}
	if client != nil {
		t.Fatal("expected nil client when http client is missing")
	}
	if !strings.Contains(err.Error(), "http client is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNew_requiresRegion(t *testing.T) {
	t.Parallel()

	_, err := New(datasource.Config{Type: Type}, http.DefaultClient)
	if err == nil {
		t.Fatal("expected error when region is missing")
	}
	if !strings.Contains(err.Error(), "region is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestQueryAndTestConnection_againstFixtureHTTP(t *testing.T) {
	t.Parallel()

	var sawGetMetricData, sawListMetrics, sawStartQuery, sawGetQueryResults, sawDescribeLogGroups bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		target := r.Header.Get("X-Amz-Target")
		path := r.URL.Path

		switch {
		case strings.Contains(path, "/operation/GetMetricData"):
			sawGetMetricData = true
			w.Header().Set("Content-Type", "application/cbor")
			w.Header().Set("Smithy-Protocol", "rpc-v2-cbor")
			_, _ = w.Write(cbor.Encode(cbor.Map{
				"MetricDataResults": cbor.List{
					cbor.Map{
						"Id":         cbor.String("m1"),
						"Label":      cbor.String("CPUUtilization"),
						"StatusCode": cbor.String("Complete"),
						"Timestamps": cbor.List{
							&cbor.Tag{ID: 1, Value: cbor.Uint(1600000000)},
						},
						"Values": cbor.List{cbor.Float64(1.5)},
					},
				},
			}))
		case strings.Contains(path, "/operation/ListMetrics"):
			sawListMetrics = true
			w.Header().Set("Content-Type", "application/cbor")
			w.Header().Set("Smithy-Protocol", "rpc-v2-cbor")
			_, _ = w.Write(cbor.Encode(cbor.Map{
				"Metrics": cbor.List{},
			}))
		case strings.Contains(target, "StartQuery"):
			sawStartQuery = true
			w.Header().Set("Content-Type", "application/x-amz-json-1.1")
			_, _ = w.Write([]byte(`{"queryId":"q-123"}`))
		case strings.Contains(target, "GetQueryResults"):
			sawGetQueryResults = true
			w.Header().Set("Content-Type", "application/x-amz-json-1.1")
			_, _ = w.Write([]byte(`{"status":"Complete","results":[[{"field":"@timestamp","value":"2020-09-13T12:26:40Z"},{"field":"@message","value":"hello error"},{"field":"level","value":"error"}]]}`))
		case strings.Contains(target, "DescribeLogGroups"):
			sawDescribeLogGroups = true
			w.Header().Set("Content-Type", "application/x-amz-json-1.1")
			_, _ = w.Write([]byte(`{"logGroups":[{"logGroupName":"/aws/lambda/my-fn"}]}`))
		default:
			t.Errorf("unexpected aws call method=%s path=%s target=%q body=%s", r.Method, path, target, body)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	client, err := New(datasource.Config{
		Type: Type,
		URL:  srv.URL,
		AuthConfig: json.RawMessage(`{
			"region":"us-east-1",
			"access_key_id":"AKID",
			"secret_access_key":"SECRET",
			"metric_namespace":"AWS/EC2",
			"log_group":"/aws/lambda/my-fn"
		}`),
	}, srv.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Unix(1600000000, 0).Add(-time.Hour)
	end := time.Unix(1600000000, 0)
	result, err := client.Query(ctx, "AWS/EC2:CPUUtilization", start, end, time.Minute, 0)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("Query status=%q error=%q", result.Status, result.Error)
	}
	if result.ResultType != "metrics" {
		t.Fatalf("ResultType=%q, want metrics", result.ResultType)
	}
	if result.Data == nil || len(result.Data.Result) != 1 {
		t.Fatalf("expected 1 series, got %+v", result.Data)
	}
	if result.Data.Result[0].Metric["metric_name"] != "CPUUtilization" {
		t.Fatalf("metric labels=%v", result.Data.Result[0].Metric)
	}
	if len(result.Data.Result[0].Values) != 1 {
		t.Fatalf("expected 1 datapoint, got %+v", result.Data.Result[0].Values)
	}
	if !sawGetMetricData {
		t.Fatal("expected fixture to receive GetMetricData")
	}

	logs, err := client.QueryWithSignal(ctx, "fields @timestamp, @message", "logs", start, end, time.Minute, 20)
	if err != nil {
		t.Fatalf("QueryWithSignal logs: %v", err)
	}
	if logs.Status != "success" || logs.ResultType != "logs" {
		t.Fatalf("logs status=%q resultType=%q error=%q", logs.Status, logs.ResultType, logs.Error)
	}
	if logs.Data == nil || len(logs.Data.Logs) != 1 {
		t.Fatalf("expected 1 log entry, got %+v", logs.Data)
	}
	if logs.Data.Logs[0].Line != "hello error" {
		t.Fatalf("log line=%q", logs.Data.Logs[0].Line)
	}
	if logs.Data.Logs[0].Level != "error" {
		t.Fatalf("log level=%q, want error", logs.Data.Logs[0].Level)
	}
	if !sawStartQuery || !sawGetQueryResults {
		t.Fatalf("expected StartQuery and GetQueryResults, start=%v get=%v", sawStartQuery, sawGetQueryResults)
	}

	if err := client.TestConnection(ctx); err != nil {
		t.Fatalf("TestConnection: %v", err)
	}
	if !sawListMetrics {
		t.Fatal("expected TestConnection to hit ListMetrics")
	}
	if !sawDescribeLogGroups {
		t.Fatal("expected TestConnection to hit DescribeLogGroups")
	}
}

func TestQueryWithSignal_rejectsUnknownSignal(t *testing.T) {
	t.Parallel()

	client, err := New(datasource.Config{
		Type:       Type,
		AuthConfig: json.RawMessage(`{"region":"us-east-1","access_key_id":"AKID","secret_access_key":"SECRET"}`),
	}, http.DefaultClient)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = client.QueryWithSignal(context.Background(), "up", "traces", time.Now().Add(-time.Hour), time.Now(), time.Minute, 0)
	if err == nil {
		t.Fatal("expected error for unknown signal")
	}
	if !strings.Contains(err.Error(), "metrics or logs") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCustomEndpoint_ignoresAmazonHosts(t *testing.T) {
	t.Parallel()

	if got := customEndpoint("https://monitoring.us-east-1.amazonaws.com"); got != "" {
		t.Fatalf("amazonaws host should use SDK default endpoints, got %q", got)
	}
	if got := customEndpoint("http://127.0.0.1:4566"); got == "" {
		t.Fatal("loopback custom endpoint should be kept")
	}
}

func TestAWSConfig_customEndpointRequiresStaticKeys(t *testing.T) {
	// Env default-chain creds must not be used for a custom BaseEndpoint.
	t.Setenv("AWS_ACCESS_KEY_ID", "DEFAULTCHAIN")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "DEFAULTCHAINSECRET")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	client, err := New(datasource.Config{
		Type:       Type,
		URL:        srv.URL,
		AuthConfig: json.RawMessage(`{"region":"us-east-1"}`),
	}, srv.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = client.awsConfig(ctx)
	if err == nil {
		t.Fatal("expected awsConfig to reject custom endpoint without static keys")
	}
	if !strings.Contains(err.Error(), "cloudwatch custom endpoint requires access_key_id and secret_access_key") {
		t.Fatalf("awsConfig error: %v", err)
	}

	if err := client.TestConnection(ctx); err == nil {
		t.Fatal("expected TestConnection to fail closed")
	} else if !strings.Contains(err.Error(), "cloudwatch custom endpoint requires access_key_id and secret_access_key") {
		t.Fatalf("TestConnection error: %v", err)
	}

	start := time.Now().Add(-time.Hour)
	end := time.Now()
	if _, err := client.Query(ctx, "AWS/EC2:CPUUtilization", start, end, time.Minute, 0); err == nil {
		t.Fatal("expected Query to fail closed")
	} else if !strings.Contains(err.Error(), "cloudwatch custom endpoint requires access_key_id and secret_access_key") {
		t.Fatalf("Query error: %v", err)
	}

	if hits != 0 {
		t.Fatalf("must not send SigV4 to custom endpoint without static keys, hits=%d", hits)
	}
}

func TestAWSConfig_amazonHostAllowsDefaultChain(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "DEFAULTCHAIN")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "DEFAULTCHAINSECRET")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	client, err := New(datasource.Config{
		Type:       Type,
		URL:        "https://monitoring.us-east-1.amazonaws.com",
		AuthConfig: json.RawMessage(`{"region":"us-east-1"}`),
	}, http.DefaultClient)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cfg, err := client.awsConfig(ctx)
	if err != nil {
		t.Fatalf("awsConfig: %v", err)
	}
	creds, err := cfg.Credentials.Retrieve(ctx)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if creds.AccessKeyID != "DEFAULTCHAIN" {
		t.Fatalf("amazon host should use default chain, got access key %q", creds.AccessKeyID)
	}
	if cfg.BaseEndpoint != nil && strings.TrimSpace(*cfg.BaseEndpoint) != "" {
		t.Fatalf("amazon host must not set BaseEndpoint, got %q", *cfg.BaseEndpoint)
	}
}
