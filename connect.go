package cloudwatch

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscw "github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	awslogs "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
)

func (c *Client) TestConnection(ctx context.Context) error {
	awsCfg, err := c.awsConfig(ctx)
	if err != nil {
		return err
	}

	metricsClient := awscw.NewFromConfig(awsCfg)
	logsClient := awslogs.NewFromConfig(awsCfg)

	shouldCheckMetrics := c.parsed.MetricNamespace != "" || len(c.parsed.LogGroups) == 0
	if shouldCheckMetrics {
		namespace := c.parsed.MetricNamespace
		if namespace == "" {
			namespace = defaultCloudWatchNamespace
		}
		if _, err := metricsClient.ListMetrics(ctx, &awscw.ListMetricsInput{
			Namespace: aws.String(namespace),
		}); err != nil {
			return fmt.Errorf("cloudwatch metrics connection test failed: %w", err)
		}
	}

	for _, groupName := range c.parsed.LogGroups {
		result, err := logsClient.DescribeLogGroups(ctx, &awslogs.DescribeLogGroupsInput{
			LogGroupNamePrefix: aws.String(groupName),
			Limit:              aws.Int32(1),
		})
		if err != nil {
			return fmt.Errorf("cloudwatch logs connection test failed: %w", err)
		}

		if len(result.LogGroups) == 0 || aws.ToString(result.LogGroups[0].LogGroupName) != groupName {
			return fmt.Errorf("configured log group %q not found", groupName)
		}
	}

	return nil
}
