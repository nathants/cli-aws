package lib

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/apigatewayv2"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/lambda"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/sqs"
)

type InfraApi struct {
	Dns    string `json:"dns,omitempty"`
	Domain string `json:"domain,omitempty"`
}

type InfraDynamoDB struct {
	Keys  []string `json:"keys,omitempty"`
	Attrs []string `json:"attrs,omitempty"`
}

type InfraVpc struct {
	name string
	Sgs  map[string][]string `json:"sgs"`
}

type InfraEC2 struct {
	name       string
	instanceID string
	Attrs      []string `json:"attrs,omitempty"`
	Count      int      `json:"count,omitempty"`
}

type InfraLambda struct {
	Policies []string `json:"policies,omitempty"`
	Allows   []string `json:"allows,omitempty"`
	Triggers []string `json:"triggers,omitempty"`
	Attrs    []string `json:"attrs,omitempty"`
}

type InfraSQS struct {
	Attrs []string `json:"attrs,omitempty"`
}

type InfraS3 struct {
	Attrs []string `json:"attrs,omitempty"`
}

type Infra struct {
	Account  string                   `json:"account"`
	Region   string                   `json:"region"`
	Api      map[string]InfraApi      `json:"api,omitempty"`
	DynamoDB map[string]InfraDynamoDB `json:"dynamodb,omitempty"`
	Vpc      map[string]InfraVpc      `json:"vpc,omitempty"`
	EC2      map[string]InfraEC2      `json:"ec2,omitempty"`
	Lambda   map[string]InfraLambda   `json:"lambda,omitempty"`
	SQS      map[string]InfraSQS      `json:"sqs,omitempty"`
	S3       map[string]InfraS3       `json:"s3,omitempty"`
}

type InfraLambdaTrigger struct {
	LambdaName   string
	TriggerType  string
	TriggerAttrs []string
}

func InfraList(ctx context.Context, filter string) (*Infra, error) {
	var err error
	infra := &Infra{}
	account, err := StsAccount(ctx)
	if err != nil {
		Logger.Fatal("error: ", err)
	}
	infra.Account = account
	infra.Region = Region()
	errs := make(chan error)
	count := 0
	triggersChan := make(chan InfraLambdaTrigger, 1024)
	run := func(fn func()) {
		go fn()
		count++
	}
	run(func() {
		infra.Api, err = InfraListApi(ctx, triggersChan)
		errs <- err
	})
	run(func() {
		infra.DynamoDB, err = InfraListDynamoDB(ctx)
		errs <- err
	})
	run(func() {
		infra.Vpc, err = InfraListVpc(ctx)
		errs <- err
	})
	run(func() {
		infra.EC2, err = InfraListEC2(ctx)
		errs <- err
	})
	run(func() {
		infra.SQS, err = InfraListSQS(ctx)
		errs <- err
	})
	run(func() {
		infra.S3, err = InfraListS3(ctx, triggersChan)
		errs <- err
	})
	run(func() {
		_, err = InfraListCloudwatch(ctx, triggersChan)
		errs <- err
	})
	lambdaErr := make(chan error)
	go func() {
		infra.Lambda, err = InfraListLambda(ctx, triggersChan, filter)
		lambdaErr <- err
	}()
	for i := 0; i < count; i++ {
		err := <-errs
		if err != nil {
			Logger.Fatal("error: ", err)
		}
	}
	close(triggersChan)
	err = <-lambdaErr
	if err != nil {
		Logger.Fatal("error: ", err)
	}
	return infra, nil
}

func InfraListCloudwatch(ctx context.Context, triggersChan chan<- InfraLambdaTrigger) (map[string]string, error) {
	rules, err := EventsListRules(ctx)
	if err != nil {
		Logger.Println("error:", err)
		return nil, err
	}
	errChan := make(chan error)
	for _, rule := range rules {
		rule := rule
		go func() {
			targets, err := EventsListRuleTargets(ctx, *rule.Name)
			if err != nil {
				errChan <- err
				return
			}
			for _, target := range targets {
				if strings.HasPrefix(*target.Arn, "arn:aws:lambda:") {
					if rule.ScheduleExpression != nil {
						triggersChan <- InfraLambdaTrigger{
							LambdaName:   Last(strings.Split(*target.Arn, ":")),
							TriggerType:  lambdaTriggerCloudwatch,
							TriggerAttrs: []string{*rule.ScheduleExpression},
						}
					}
				}
			}
			errChan <- nil
		}()
	}
	for range rules {
		err := <-errChan
		if err != nil {
			Logger.Println("error:", err)
			return nil, err
		}
	}
	return nil, nil
}

func InfraListLambda(ctx context.Context, triggersChan <-chan InfraLambdaTrigger, filter string) (map[string]InfraLambda, error) {
	allFns, err := LambdaListFunctions(ctx)
	if err != nil {
		Logger.Println("error:", err)
		return nil, err
	}
	var fns []*lambda.FunctionConfiguration
	for _, fn := range allFns {
		if filter != "" && !strings.Contains(*fn.FunctionName, filter) {
			continue
		}
		fns = append(fns, fn)
	}
	errChan := make(chan error)
	triggers := make(map[string][]InfraLambdaTrigger)
	res := make(map[string]InfraLambda)
	for _, fn := range fns {
		fn := fn
		go func() {
			l := InfraLambda{}
			if *fn.MemorySize != 128 { // default
				l.Attrs = append(l.Attrs, fmt.Sprintf("memory %d", *fn.MemorySize))
			}
			if *fn.Timeout != 3 { // default
				l.Attrs = append(l.Attrs, fmt.Sprintf("timeout %d", *fn.Timeout))
			}
			out, err := LambdaClient().GetFunctionConcurrencyWithContext(ctx, &lambda.GetFunctionConcurrencyInput{
				FunctionName: aws.String(*fn.FunctionName),
			})
			if err != nil {
				errChan <- err
				return
			}
			if out.ReservedConcurrentExecutions != nil {
				l.Attrs = append(l.Attrs, fmt.Sprintf("concurrency %d", *out.ReservedConcurrentExecutions))
			}
			roleName := Last(strings.Split(*fn.Role, "/"))
			policies, err := IamListRolePolicies(ctx, roleName)
			if err != nil {
				errChan <- err
				return
			}
			for _, policy := range policies {
				l.Policies = append(l.Policies, *policy.PolicyName)
			}
			allows, err := IamListRoleAllows(ctx, roleName)
			if err != nil {
				errChan <- err
				return
			}
			for _, allow := range allows {
				l.Allows = append(l.Allows, allow.String())
			}
			var marker *string
			for {
				out, err := LambdaClient().ListEventSourceMappingsWithContext(ctx, &lambda.ListEventSourceMappingsInput{
					FunctionName: fn.FunctionArn,
					Marker:       marker,
				})
				if err != nil {
					errChan <- err
					return
				}
				for _, mapping := range out.EventSourceMappings {
					if Contains([]string{"Disabled", "Disabling"}, *mapping.State) {
						continue
					}
					infra := ArnToInfraName(*mapping.EventSourceArn)
					switch infra {
					case lambdaTriggerSQS, lambdaTriggerDynamoDB:
						var sourceName string
						switch infra {
						case lambdaTriggerSQS:
							sourceName = SQSArnToName(*mapping.EventSourceArn)
						case lambdaTriggerDynamoDB:
							sourceName = DynamoDBStreamArnToTableName(*mapping.EventSourceArn)
						default:
							err := fmt.Errorf("unknown infra: %s", infra)
							errChan <- err
							return
						}
						triggers[*fn.FunctionName] = append(triggers[*fn.FunctionName], InfraLambdaTrigger{
							LambdaName:  *fn.FunctionName,
							TriggerType: infra,
							TriggerAttrs: []string{
								sourceName,
								fmt.Sprintf("BatchSize=%d", *mapping.BatchSize),
								fmt.Sprintf("ParallelizationFactor=%d", *mapping.ParallelizationFactor),
								fmt.Sprintf("MaximumRetryAttempts=%d", *mapping.MaximumRetryAttempts),
								fmt.Sprintf("StartingPosition=%s", *mapping.StartingPosition),
								fmt.Sprintf("MaximumBatchingWindowInSeconds=%d", *mapping.MaximumBatchingWindowInSeconds),
							},
						})
					default:
						Logger.Println("ignoring event source mapping:", *mapping.FunctionArn, *mapping.EventSourceArn)
					}
				}
				if out.NextMarker == nil {
					break
				}
				marker = out.NextMarker
			}
			res[*fn.FunctionName] = l
			errChan <- nil
		}()
	}
	for range fns {
		err := <-errChan
		if err != nil {
			Logger.Println("error:", err)
			return nil, err
		}
	}
	for trigger := range triggersChan {
		triggers[trigger.LambdaName] = append(triggers[trigger.LambdaName], trigger)
	}
	for _, fn := range fns {
		ts, ok := triggers[*fn.FunctionName]
		if ok {
			for _, trigger := range ts {
				val := trigger.TriggerType
				if len(trigger.TriggerAttrs) > 0 {
					val += " " + strings.Join(trigger.TriggerAttrs, " ")
				}
				l, ok := res[*fn.FunctionName]
				if !ok {
					panic(*fn.FunctionName)
				}
				l.Triggers = append(l.Triggers, val)
				res[*fn.FunctionName] = l
			}
		}
	}
	return res, nil
}

func InfraListApi(ctx context.Context, triggersChan chan<- InfraLambdaTrigger) (map[string]InfraApi, error) {
	infraApis := make(map[string]InfraApi)
	lock := &sync.Mutex{}
	apis, err := ApiList(ctx)
	if err != nil {
		Logger.Println("error:", err)
		return nil, err
	}
	domains, err := ApiListDomains(ctx)
	if err != nil {
		Logger.Println("error:", err)
		return nil, err
	}
	apiToDomain := make(map[string]string)
	for _, domain := range domains {
		mappings, err := ApiClient().GetApiMappingsWithContext(ctx, &apigatewayv2.GetApiMappingsInput{
			DomainName: domain.DomainName,
			MaxResults: aws.String(fmt.Sprint(500)),
		})
		if err != nil {
			Logger.Println("error:", err)
			return nil, err
		}
		if len(mappings.Items) == 500 {
			err := fmt.Errorf("api overflow without pagination")
			Logger.Println("error:", err)
			return nil, err
		}
		for _, mapping := range mappings.Items {
			if *mapping.Stage == lambdaDollarDefault {
				apiToDomain[*mapping.ApiId] = *domain.DomainName
			}
		}
	}
	zones, err := Route53ListZones(ctx)
	if err != nil {
		Logger.Println("error:", err)
		return nil, err
	}
	apiToDns := make(map[string]string)
	for _, zone := range zones {
		records, err := Route53ListRecords(ctx, *zone.Id)
		if err != nil {
			Logger.Println("error:", err)
			return nil, err
		}
		for _, record := range records {
			if record.Name != nil {
				domain := strings.TrimRight(*record.Name, ".")
				mappings, err := ApiClient().GetApiMappingsWithContext(ctx, &apigatewayv2.GetApiMappingsInput{
					DomainName: aws.String(domain),
					MaxResults: aws.String(fmt.Sprint(500)),
				})
				if err != nil {
					aerr, ok := err.(awserr.Error)
					if !ok || aerr.Code() != apigatewayv2.ErrCodeNotFoundException {
						Logger.Println("error:", err)
						return nil, err
					}
				}
				if len(mappings.Items) == 500 {
					err := fmt.Errorf("api overflow without pagination")
					Logger.Println("error:", err)
					return nil, err
				}
				for _, mapping := range mappings.Items {
					if *mapping.Stage == lambdaDollarDefault {
						apiToDns[*mapping.ApiId] = domain
					}
				}
			}
		}
	}
	errChan := make(chan error)
	for _, api := range apis {
		api := api
		go func() {
			infraApi := InfraApi{}
			out, err := ApiClient().GetIntegrationsWithContext(ctx, &apigatewayv2.GetIntegrationsInput{
				ApiId:      api.ApiId,
				MaxResults: aws.String(fmt.Sprint(500)),
			})
			if err != nil {
				errChan <- err
				return
			}
			if len(out.Items) != 1 {
				errChan <- nil
				return
			}
			attrs := []string{}
			dns, ok := apiToDns[*api.ApiId]
			if ok {
				attrs = append(attrs, fmt.Sprintf("dns=%s", dns))
				infraApi.Dns = dns
			} else {
				domain, ok := apiToDomain[*api.ApiId]
				if ok {
					attrs = append(attrs, fmt.Sprintf("domain=%s", domain))
					infraApi.Domain = domain
				}
			}
			triggerType := lambdaTriggerApi
			lambdaName := *api.Name
			if api.RouteSelectionExpression != nil && *api.RouteSelectionExpression == lambdaRouteSelection { // websocket uses a suffix in addition to the lambda name
				if !strings.HasSuffix(lambdaName, LambdaWebsocketSuffix) {
					Logger.Println(*api.RouteSelectionExpression)
					panic(lambdaName)
				}
				lambdaName = lambdaName[:len(lambdaName)-len(LambdaWebsocketSuffix)]
				triggerType = lambdaTriggerWebsocket
			}
			triggersChan <- InfraLambdaTrigger{
				LambdaName:   lambdaName,
				TriggerType:  triggerType,
				TriggerAttrs: attrs,
			}
			lock.Lock()
			infraApis[*api.Name] = infraApi
			lock.Unlock()
			errChan <- nil
		}()
	}
	for range apis {
		err := <-errChan
		if err != nil {
			Logger.Println("error:", err)
			return nil, err
		}
	}
	return infraApis, nil
}

func InfraListDynamoDB(ctx context.Context) (map[string]InfraDynamoDB, error) {
	lock := &sync.Mutex{}
	infraDynamoDB := make(map[string]InfraDynamoDB)
	tableNames, err := DynamoDBListTables(ctx)
	if err != nil {
		Logger.Println("error:", err)
		return nil, err
	}
	errChan := make(chan error)
	for _, tableName := range tableNames {
		tableName := tableName
		go func() {
			db := InfraDynamoDB{}
			out, err := DynamoDBClient().DescribeTableWithContext(ctx, &dynamodb.DescribeTableInput{
				TableName: aws.String(tableName),
			})
			if err != nil {
				errChan <- err
				return
			}
			attrTypes := make(map[string]string)
			for _, attr := range out.Table.AttributeDefinitions {
				attrTypes[*attr.AttributeName] = *attr.AttributeType
			}
			for _, key := range out.Table.KeySchema {
				db.Keys = append(db.Keys, fmt.Sprintf("%s:%s:%s", *key.AttributeName, strings.ToLower(attrTypes[*key.AttributeName]), strings.ToLower(*key.KeyType)))
			}
			if *out.Table.ProvisionedThroughput.ReadCapacityUnits != 0 {
				db.Attrs = append(db.Attrs, fmt.Sprintf("ProvisionedThroughput.ReadCapacityUnits=%d", *out.Table.ProvisionedThroughput.ReadCapacityUnits))
			}
			if *out.Table.ProvisionedThroughput.WriteCapacityUnits != 0 {
				db.Attrs = append(db.Attrs, fmt.Sprintf("ProvisionedThroughput.WriteCapacityUnits=%d", *out.Table.ProvisionedThroughput.WriteCapacityUnits))
			}
			if out.Table.StreamSpecification != nil {
				db.Attrs = append(db.Attrs, fmt.Sprintf("StreamSpecification.StreamViewType=%s", *out.Table.StreamSpecification.StreamViewType))
			}
			for i, index := range out.Table.LocalSecondaryIndexes {
				db.Attrs = append(db.Attrs, fmt.Sprintf("LocalSecondaryIndexes.%d.IndexName=%s", i, *index.IndexName))
				for j, key := range index.KeySchema {
					db.Attrs = append(db.Attrs, fmt.Sprintf("LocalSecondaryIndexes.%d.Key.%d=%s:%s:%s", i, j, *key.AttributeName, attrTypes[*key.AttributeName], *key.KeyType))
				}
				db.Attrs = append(db.Attrs, fmt.Sprintf("LocalSecondaryIndexes.%d.Projection.ProjectionType=%s", i, *index.Projection.ProjectionType))
				for j, attr := range index.Projection.NonKeyAttributes {
					db.Attrs = append(db.Attrs, fmt.Sprintf("LocalSecondaryIndexes.%d.Projection.NonKeyAttributes.%d=%s", i, j, *attr))
				}
			}
			for i, index := range out.Table.GlobalSecondaryIndexes {
				db.Attrs = append(db.Attrs, fmt.Sprintf("GlobalSecondaryIndexes.%d.IndexName=%s", i, *index.IndexName))
				for j, key := range index.KeySchema {
					db.Attrs = append(db.Attrs, fmt.Sprintf("GlobalSecondaryIndexes.%d.Key.%d=%s:%s:%s", i, j, *key.AttributeName, attrTypes[*key.AttributeName], *key.KeyType))
				}
				db.Attrs = append(db.Attrs, fmt.Sprintf("GlobalSecondaryIndexes.%d.Projection.ProjectionType=%s", i, *index.Projection.ProjectionType))
				for j, attr := range index.Projection.NonKeyAttributes {
					db.Attrs = append(db.Attrs, fmt.Sprintf("GlobalSecondaryIndexes.%d.Projection.NonKeyAttributes.%d=%s", i, j, *attr))
				}
				db.Attrs = append(db.Attrs, fmt.Sprintf("GlobalSecondaryIndexes.%d.ProvisionedThroughput.ReadCapacityUnits=%d", i, *index.ProvisionedThroughput.ReadCapacityUnits))
				db.Attrs = append(db.Attrs, fmt.Sprintf("GlobalSecondaryIndexes.%d.ProvisionedThroughput.WriteCapacityUnits=%d", i, *index.ProvisionedThroughput.WriteCapacityUnits))
			}
			tags, err := DynamoDBListTags(ctx, tableName)
			if err != nil {
				errChan <- err
				return
			}
			for i, tag := range tags {
				db.Attrs = append(db.Attrs, fmt.Sprintf("Tags.%d.Key=%s", i, *tag.Key))
				db.Attrs = append(db.Attrs, fmt.Sprintf("Tags.%d.Value=%s", i, *tag.Value))
			}
			lock.Lock()
			infraDynamoDB[tableName] = db
			lock.Unlock()
			errChan <- nil
		}()
	}
	for range tableNames {
		err := <-errChan
		if err != nil {
			Logger.Println("error:", err)
			return nil, err
		}
	}
	return infraDynamoDB, nil
}

func InfraListVpc(ctx context.Context) (map[string]InfraVpc, error) {
	infraVpc := make(map[string]InfraVpc)
	sgsByVpcId := make(map[string][]*ec2.SecurityGroup)
	vpcs, err := VpcList(ctx)
	if err != nil {
		Logger.Println("error:", err)
		return nil, err
	}
	sgs, err := EC2ListSgs(ctx)
	if err != nil {
		Logger.Println("error:", err)
		return nil, err
	}
	for _, sg := range sgs {
		sgsByVpcId[*sg.VpcId] = append(sgsByVpcId[*sg.VpcId], sg)
	}
	for _, vpc := range vpcs {
		val := InfraVpc{}
		val.name = EC2Name(vpc.Tags)
		val.Sgs = make(map[string][]string)
		for _, sg := range sgsByVpcId[*vpc.VpcId] {
			sgName := *sg.GroupName
			for _, p := range sg.IpPermissions {
				rs, err := EC2SgRules(p)
				if err != nil {
					Logger.Println("error:", err)
					return nil, err
				}
				for _, r := range rs {
					val.Sgs[sgName] = append(val.Sgs[sgName], r.String())
				}
			}
		}
		infraVpc[val.name] = val
	}
	return infraVpc, nil
}

func InfraListEC2(ctx context.Context) (map[string]InfraEC2, error) {
	infraEC2 := make(map[string]InfraEC2)
	instances, err := EC2ListInstances(ctx, nil, "running")
	if err != nil {
		Logger.Println("error:", err)
		return nil, err
	}
	ec2s := make(map[string][]InfraEC2)
	for _, instance := range instances {
		ec2 := InfraEC2{}
		ec2.name = EC2Name(instance.Tags)
		ec2.instanceID = *instance.InstanceId
		ec2.Count = 1
		ec2.Attrs = append(ec2.Attrs, fmt.Sprintf("Type=%s", *instance.InstanceType))
		ec2.Attrs = append(ec2.Attrs, fmt.Sprintf("Image=%s", *instance.ImageId))
		ec2.Attrs = append(ec2.Attrs, fmt.Sprintf("Kind=%s", EC2Kind(instance)))
		ec2.Attrs = append(ec2.Attrs, fmt.Sprintf("Vpc=%s", *instance.VpcId))
		for _, tag := range instance.Tags {
			if *tag.Key != "creation-date" && *tag.Key != "Name" && *tag.Key != "aws:ec2spot:fleet-request-id" {
				ec2.Attrs = append(ec2.Attrs, fmt.Sprintf("Tags.%s=%s", *tag.Key, *tag.Value))
			}
		}
		key := ec2.name + "::" + strings.Join(ec2.Attrs, "::")
		ec2s[key] = append(ec2s[key], ec2)
	}
	var keys []string
	for k := range ec2s {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return len(ec2s[keys[i]]) > len(ec2s[keys[j]])
	})
	for _, k := range keys {
		vs := ec2s[k]
		ec2 := vs[0]
		ec2.Count = len(vs)
		name := strings.Split(k, "::")[0]
		_, ok := infraEC2[name]
		if !ok {
			infraEC2[name] = ec2
		} else {
			attrs := []string{fmt.Sprintf("Name=%s", name)}
			attrs = append(attrs, ec2.Attrs...)
			ec2.Attrs = attrs
			infraEC2[ec2.instanceID] = ec2
		}
	}
	return infraEC2, nil
}

func InfraListS3(ctx context.Context, triggersChan chan<- InfraLambdaTrigger) (map[string]InfraS3, error) {
	lock := &sync.Mutex{}
	res := make(map[string]InfraS3)
	buckets, err := S3Client().ListBucketsWithContext(ctx, &s3.ListBucketsInput{})
	if err != nil {
		Logger.Println("error:", err)
		return nil, err
	}
	errChan := make(chan error)
	for _, bucket := range buckets.Buckets {
		bucket := bucket
		go func() {
			s := InfraS3{}
			descr, err := S3GetBucketDescription(ctx, *bucket.Name)
			if err != nil {
				errChan <- err
				return
			}
			s3Default := s3EnsureInputDefault()
			if descr.Policy == nil && s3Default.acl != "private" {
				s.Attrs = append(s.Attrs, "acl=private")
			} else if descr.Policy != nil && reflect.DeepEqual(s3PublicPolicy(*bucket.Name), *descr.Policy) && s3Default.acl != "public" {
				s.Attrs = append(s.Attrs, "acl=public")
			}
			if descr.Cors == nil && s3Default.cors {
				s.Attrs = append(s.Attrs, "cors=false")
			} else if reflect.DeepEqual(s3Cors, descr.Cors) {
				s.Attrs = append(s.Attrs, "cors=true")
			}
			if descr.Versioning != s3Default.versioning {
				s.Attrs = append(s.Attrs, fmt.Sprintf("versioning=%t", descr.Versioning))
			}
			encryption := reflect.DeepEqual(descr.Encryption, s3EncryptionConfig)
			if encryption != s3Default.encryption {
				s.Attrs = append(s.Attrs, fmt.Sprintf("encryption=%t", encryption))
			}
			metrics := descr.Metrics != nil
			if s3Default.metrics != metrics {
				s.Attrs = append(s.Attrs, fmt.Sprintf("metrics=%t", metrics))
			}
			ttl := descr.Lifecycle
			if len(ttl) == 1 && int64(s3Default.ttlDays) != *ttl[0].Expiration.Days {
				s.Attrs = append(s.Attrs, fmt.Sprintf("ttldays=%d", *ttl[0].Expiration.Days))
			}
			if descr.Notifications != nil {
				for _, conf := range descr.Notifications.LambdaFunctionConfigurations {
					triggersChan <- InfraLambdaTrigger{
						LambdaName:   LambdaArnToLambdaName(*conf.LambdaFunctionArn),
						TriggerType:  lambdaTrigerS3,
						TriggerAttrs: []string{*bucket.Name},
					}
				}
			}
			lock.Lock()
			res[*bucket.Name] = s
			lock.Unlock()
			errChan <- nil
		}()
	}
	for range buckets.Buckets {
		err := <-errChan
		if err != nil {
			Logger.Println("error:", err)
			return nil, err
		}
	}
	return res, nil
}

func InfraListSQS(ctx context.Context) (map[string]InfraSQS, error) {
	urls, err := SQSListQueueUrls(ctx)
	if err != nil {
		Logger.Println("error:", err)
		return nil, err
	}
	errChan := make(chan error)
	lock := &sync.Mutex{}
	res := make(map[string]InfraSQS)
	for _, url := range urls {
		url := url
		go func() {
			out, err := SQSClient().GetQueueAttributesWithContext(ctx, &sqs.GetQueueAttributesInput{
				QueueUrl: aws.String(url),
				AttributeNames: []*string{
					aws.String("DelaySeconds"),
					aws.String("MaximumMessageSize"),
					aws.String("MessageRetentionPeriod"),
					aws.String("ReceiveMessageWaitTimeSeconds"),
					aws.String("VisibilityTimeout"),
					aws.String("KmsDataKeyReusePeriodSeconds"),
				},
			})
			if err != nil {
				errChan <- err
				return
			}
			s := InfraSQS{}
			if out.Attributes["DelaySeconds"] != nil && *out.Attributes["DelaySeconds"] != "0" { // default
				s.Attrs = append(s.Attrs, "DelaySeconds="+*out.Attributes["DelaySeconds"])
			}
			if out.Attributes["MaximumMessageSize"] != nil && *out.Attributes["MaximumMessageSize"] != "262144" { // default
				s.Attrs = append(s.Attrs, "MaximumMessageSize="+*out.Attributes["MaximumMessageSize"])
			}
			if out.Attributes["MessageRetentionPeriod"] != nil && *out.Attributes["MessageRetentionPeriod"] != "345600" { // default
				s.Attrs = append(s.Attrs, "MessageRetentionPeriod="+*out.Attributes["MessageRetentionPeriod"])
			}
			if out.Attributes["ReceiveMessageWaitTimeSeconds"] != nil && *out.Attributes["ReceiveMessageWaitTimeSeconds"] != "0" { // default
				s.Attrs = append(s.Attrs, "ReceiveMessageWaitTimeSeconds="+*out.Attributes["ReceiveMessageWaitTimeSeconds"])
			}
			if out.Attributes["VisibilityTimeout"] != nil && *out.Attributes["VisibilityTimeout"] != "30" { // default
				s.Attrs = append(s.Attrs, "VisibilityTimeout="+*out.Attributes["VisibilityTimeout"])
			}
			if out.Attributes["KmsDataKeyReusePeriodSeconds"] != nil && *out.Attributes["KmsDataKeyReusePeriodSeconds"] != "300" { // default
				s.Attrs = append(s.Attrs, "KmsDataKeyReusePeriodSeconds="+*out.Attributes["KmsDataKeyReusePeriodSeconds"])
			}
			lock.Lock()
			res[SQSUrlToName(url)] = s
			lock.Unlock()
			errChan <- nil
		}()
	}
	for range urls {
		err := <-errChan
		if err != nil {
			Logger.Println("error:", err)
			return nil, err
		}
	}
	return res, nil
}

func InfraEnsureS3(ctx context.Context, buckets []string, preview bool) error {
	for _, bucket := range buckets {
		parts := strings.Split(bucket, " ")
		name := parts[0]
		attrs := parts[1:]
		input, err := S3EnsureInput(name, attrs)
		if err != nil {
			Logger.Println("error:", err)
			return err
		}
		err = S3Ensure(ctx, input, preview)
		if err != nil {
			Logger.Println("error:", err)
			return err
		}
	}
	return nil
}

func InfraEnsureDynamoDB(ctx context.Context, dbs []string, preview bool) error {
	for _, db := range dbs {
		parts := strings.Split(db, " ")
		name := parts[0]
		var keys []string
		var attrs []string
		for _, part := range parts[1:] {
			if strings.Contains(part, "=") {
				attrs = append(attrs, part)
			} else {
				keys = append(keys, part)
			}
		}
		input, err := DynamoDBEnsureInput(name, keys, attrs)
		if err != nil {
			Logger.Println("error:", err)
			return err
		}
		err = DynamoDBEnsure(ctx, input, preview)
		if err != nil {
			Logger.Println("error:", err)
			return err
		}
	}
	return nil
}

func InfraEnsureSqs(ctx context.Context, queues []string, preview bool) error {
	for _, queue := range queues {
		parts := strings.Split(queue, "/")
		name := parts[0]
		attrs := parts[1:]
		input, err := SQSEnsureInput(name, attrs)
		if err != nil {
			Logger.Println("error:", err)
			return err
		}
		err = SQSEnsure(ctx, input, preview)
		if err != nil {
			Logger.Println("error:", err)
			return err
		}
	}
	return nil
}
