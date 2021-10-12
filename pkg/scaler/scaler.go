package scaler

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbTypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	log "github.com/sirupsen/logrus"
)

type capacityType string

const readCapacity = "read"
const writeCapacity = "write"

type Scaler struct {
	cwClient       *cloudwatch.Client
	ddbClient      *dynamodb.Client
	ScalingTargets []TableScalingConfiguration
}

type TableScalingConfiguration struct {
	TableName           string
	ReadUpperBound      float64
	WriteUpperBound     float64
	ReadLowerBound      float64
	WriteLowerBound     float64
	ReadBufferCapacity  float64
	WriteBufferCapacity float64
}

func New(cfg aws.Config, scalingTargets []TableScalingConfiguration) *Scaler {
	return &Scaler{
		cwClient:       cloudwatch.NewFromConfig(cfg),
		ddbClient:      dynamodb.NewFromConfig(cfg),
		ScalingTargets: scalingTargets,
	}
}

func (s *Scaler) Run(ctx context.Context) {
	log.Debugf("config: %+v", s.ScalingTargets)

	ticker := time.NewTicker(1 * time.Minute)

	s.scaleTargets(ctx)

	for {
		select {
		case <-ctx.Done():
			log.Infof("stopping execution: %s", ctx.Err())
			return
		case <-ticker.C:
			s.scaleTargets(ctx)
		}
	}
}

func (s *Scaler) scaleTargets(ctx context.Context) {
	for _, target := range s.ScalingTargets {
		err := s.scaleTarget(ctx, target)
		if err != nil {
			log.Warn(err)
		}
	}
}

func (s *Scaler) scaleTarget(ctx context.Context, target TableScalingConfiguration) error {
	table, err := s.ddbClient.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(target.TableName)})
	if err != nil {
		return fmt.Errorf("skipping scaling action: failed to get table status for %s: %w", target.TableName, err)
	}

	if table.Table.TableStatus != "ACTIVE" {
		return fmt.Errorf("skipping scaling action: table %s is currently in status: %s", target.TableName, table.Table.TableStatus)
	}

	tableCap, err := s.getTableCapacity(ctx, target.TableName)
	if err != nil {
		return fmt.Errorf("failed to get table capacity for %s: %w", target.TableName, err)
	}

	tableCap.readCapacity.provisioned = table.Table.ProvisionedThroughput.ReadCapacityUnits
	tableCap.writeCapacity.provisioned = table.Table.ProvisionedThroughput.WriteCapacityUnits

	if !tableCap.isSafe() {
		return fmt.Errorf("table capacity is not safe, skipping table %s: %+v", target.TableName, *tableCap)
	}

	var newReadCap, newWriteCap int64

	if tableCap.readCapacity.throttles > 0 && tableCap.readCapacity.provisioned != nil {
		newReadCap, err = makeThrottlingScalingDecision(tableCap.readCapacity, target)
		if err != nil {
			return fmt.Errorf("skipping scaling action: failed to make read throttle scaling decision for %s: %w", target.TableName, err)
		}
	}

	if tableCap.writeCapacity.throttles > 0 && tableCap.writeCapacity.provisioned != nil {
		newWriteCap, err = makeThrottlingScalingDecision(tableCap.writeCapacity, target)
		if err != nil {
			return fmt.Errorf("skipping scaling action: failed to make write throttle scaling decision for %s: %w", target.TableName, err)
		}
	}

	if newReadCap == 0 {
		newReadCap, err = makeLowConsumptionScalingDecision(tableCap.readCapacity, target)
		if err != nil {
			return fmt.Errorf("skipping scaling action: failed to make read low consumption scaling decision for %s: %w", target.TableName, err)
		}
	}

	if newWriteCap == 0 {
		newWriteCap, err = makeLowConsumptionScalingDecision(tableCap.writeCapacity, target)
		if err != nil {
			return fmt.Errorf("skipping scaling action: failed to make write low consumption scaling decision for %s: %w", target.TableName, err)
		}
	}

	if (newReadCap >= 1 || newWriteCap >= 1) &&
		(newReadCap != *table.Table.ProvisionedThroughput.ReadCapacityUnits || newWriteCap != *table.Table.ProvisionedThroughput.WriteCapacityUnits) {
		err = s.updateTableCapacity(ctx, table.Table, newReadCap, newWriteCap)
		if err != nil {
			return fmt.Errorf("failed to scale table %s: %w", target.TableName, err)
		}
	}

	log.Debugf("no scaling necessary for %s", target.TableName)

	return nil
}

func (s *Scaler) updateTableCapacity(ctx context.Context, table *dynamodbTypes.TableDescription, newReadCap int64, newWriteCap int64) error {
	provisionThroughput := dynamodbTypes.ProvisionedThroughput{
		ReadCapacityUnits:  table.ProvisionedThroughput.ReadCapacityUnits,
		WriteCapacityUnits: table.ProvisionedThroughput.WriteCapacityUnits,
	}

	if newReadCap > 0 {
		provisionThroughput.ReadCapacityUnits = aws.Int64(newReadCap)
	}

	if newWriteCap > 0 {
		provisionThroughput.WriteCapacityUnits = aws.Int64(newWriteCap)
	}

	_, err := s.ddbClient.UpdateTable(ctx, &dynamodb.UpdateTableInput{
		TableName:             table.TableName,
		ProvisionedThroughput: &provisionThroughput,
	})
	if err != nil {
		return fmt.Errorf("failed to update table: %s", err)
	}

	log.Infof("updated table capacity for %s: read=%d,write=%d", *table.TableName, newReadCap, newWriteCap)

	return nil
}

func makeThrottlingScalingDecision(c capacity, target TableScalingConfiguration) (int64, error) {
	switch c.capType {
	case readCapacity:
		result := (c.throttles + float64(*c.provisioned)) * (1 + target.ReadBufferCapacity)
		if result > 40000 {
			result = 40000
		}

		if result > target.ReadUpperBound {
			result = target.ReadUpperBound
		}

		return int64(math.Round(result)), nil
	case writeCapacity:
		result := (c.throttles + float64(*c.provisioned)) * (1 + target.WriteBufferCapacity)
		if result > 40000 {
			result = 40000
		}

		if result > target.WriteUpperBound {
			result = target.WriteUpperBound
		}

		return int64(math.Round(result)), nil
	default:
		return 0, fmt.Errorf("unknown capacity type: %s", c.capType)
	}
}

func makeLowConsumptionScalingDecision(c capacity, target TableScalingConfiguration) (int64, error) {
	switch c.capType {
	case readCapacity:
		bufferedTargetCap := *c.consumed * (1 + target.ReadBufferCapacity)

		if bufferedTargetCap < target.ReadLowerBound {
			return int64(target.ReadLowerBound), nil
		}

		if bufferedTargetCap < float64(*c.provisioned) {
			return int64(math.Round(bufferedTargetCap)), nil
		}

		return *c.provisioned, nil
	case writeCapacity:
		bufferedTargetCap := *c.consumed * (1 + target.ReadBufferCapacity)

		if bufferedTargetCap < target.WriteLowerBound {
			return int64(target.WriteLowerBound), nil
		}

		if bufferedTargetCap < float64(*c.provisioned) {
			return int64(math.Round(bufferedTargetCap)), nil
		}

		return *c.provisioned, nil
	default:
		return 0, fmt.Errorf("unknown capacity type: %s", c.capType)
	}
}
