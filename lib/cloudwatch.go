package lib

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/cloudwatch"
	"github.com/gofrs/uuid"
)

var cloudwatchClient *cloudwatch.CloudWatch
var cloudwatchClientLock sync.RWMutex

func CloudwatchClientExplicit(accessKeyID, accessKeySecret, region string) *cloudwatch.CloudWatch {
	return cloudwatch.New(SessionExplicit(accessKeyID, accessKeySecret, region))
}

func CloudwatchClient() *cloudwatch.CloudWatch {
	cloudwatchClientLock.Lock()
	defer cloudwatchClientLock.Unlock()
	if cloudwatchClient == nil {
		cloudwatchClient = cloudwatch.New(Session())
	}
	return cloudwatchClient
}

func CloudwatchListMetrics(ctx context.Context, namespace, metric *string) ([]*cloudwatch.Metric, error) {
	_ = aws.String
	var token *string
	var metrics []*cloudwatch.Metric
	for {
		out, err := CloudwatchClient().ListMetricsWithContext(ctx, &cloudwatch.ListMetricsInput{
			NextToken:  token,
			Namespace:  namespace,
			MetricName: metric,
		})
		if err != nil {
			Logger.Println("error:", err)
			return nil, err
		}
		metrics = append(metrics, out.Metrics...)
		if out.NextToken == nil {
			break
		}
		token = out.NextToken
	}
	return metrics, nil
}

func CloudwatchGetMetricData(ctx context.Context, period int, stat string, fromTime, toTime *time.Time, namespace string, metrics []string, dimension string) ([]*cloudwatch.MetricDataResult, error) {
	var token *string
	var result []*cloudwatch.MetricDataResult
	for {

		input := &cloudwatch.GetMetricDataInput{
			EndTime:           toTime,
			StartTime:         fromTime,
			NextToken:         token,
			MetricDataQueries: []*cloudwatch.MetricDataQuery{},
		}

		for _, metric := range metrics {
			input.MetricDataQueries = append(input.MetricDataQueries, &cloudwatch.MetricDataQuery{
				Id: aws.String("a" + strings.ReplaceAll(uuid.Must(uuid.NewV4()).String(), "-", "")),
				MetricStat: &cloudwatch.MetricStat{
					Period: aws.Int64(int64(period)),
					Stat:   aws.String(stat),
					Metric: &cloudwatch.Metric{
						Namespace:  aws.String(namespace),
						MetricName: aws.String(metric),
						Dimensions: []*cloudwatch.Dimension{{
							Name:  aws.String(strings.Split(dimension, "=")[0]),
							Value: aws.String(strings.Split(dimension, "=")[1]),
						}},
					},
				},
			})
		}

		out, err := CloudwatchClient().GetMetricDataWithContext(ctx, input)
		if err != nil {
			Logger.Println("error:", err)
			return nil, err
		}
		result = append(result, out.MetricDataResults...)
		if out.NextToken == nil {
			break
		}
		token = out.NextToken
	}
	return result, nil
}
