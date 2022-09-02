/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package instances

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/awslabs/kubernetes-iteration-toolkit/operator/pkg/apis/dataplane/v1alpha1"
	"github.com/awslabs/kubernetes-iteration-toolkit/operator/pkg/awsprovider"
	"github.com/awslabs/kubernetes-iteration-toolkit/operator/pkg/awsprovider/launchtemplate"
	"github.com/awslabs/kubernetes-iteration-toolkit/operator/pkg/kubeprovider"
	"github.com/awslabs/kubernetes-iteration-toolkit/operator/pkg/utils/functional"
	cpinstances "github.com/awslabs/kubernetes-iteration-toolkit/operator/pkg/utils/instances"
	"go.uber.org/zap"

	"knative.dev/pkg/ptr"
)

type Controller struct {
	ec2api      *awsprovider.EC2
	autoscaling *awsprovider.AutoScaling
	instances   *cpinstances.Provider
}

// NewController returns a controller for managing LaunchTemplates and ASG in AWS
func NewController(ec2api *awsprovider.EC2, autoscaling *awsprovider.AutoScaling, client *kubeprovider.Client) *Controller {
	return &Controller{ec2api: ec2api, autoscaling: autoscaling, instances: cpinstances.New(client)}
}

func (c *Controller) Reconcile(ctx context.Context, dataplane *v1alpha1.DataPlane) error {
	subnets, err := c.subnetsFor(ctx, &dataplane.Spec)
	if err != nil {
		return fmt.Errorf("getting subnet for %s, %w", dataplane.Spec.ClusterName, err)
	}
	return c.CreateResource(ctx, dataplane.Name, &dataplane.Spec, subnets)
}

func (c *Controller) CreateResource(ctx context.Context, name string, spec *v1alpha1.DataPlaneSpec, subnets []string) error {
	asg, err := c.getAutoScalingGroup(ctx, AutoScalingGroupNameFor(spec.ClusterName, name))
	if err != nil {
		return fmt.Errorf("getting auto scaling group for %v, %w", spec.ClusterName, err)
	}
	if asg == nil {
		if err := c.createAutoScalingGroup(ctx, name, spec, subnets); err != nil {
			return fmt.Errorf("creating auto scaling group for %v, %w", spec.ClusterName, err)
		}
		zap.S().Infof("[%s] Created autoscaling group", spec.ClusterName)
		return nil
	}
	if asg.Status != nil && *asg.Status == "Delete in progress" {
		// there are scenarios if you delete ASG and recreate quickly ASG might still be getting deleted
		return fmt.Errorf("ASG %v deletion in progress", ptr.StringValue(asg.AutoScalingGroupName))
	}
	if err := c.updateAutoScalingGroup(ctx, name, spec, subnets, asg); err != nil {
		return fmt.Errorf("updating auto scaling group %v, %w", AutoScalingGroupNameFor(spec.ClusterName, name), err)
	}
	return nil
}

func (c *Controller) Finalize(ctx context.Context, dataplane *v1alpha1.DataPlane) error {
	return c.DeleteResource(ctx, dataplane.Spec.ClusterName, dataplane.Name)
}

func (c *Controller) DeleteResource(ctx context.Context, clusterName string, dataplaneName string) error {
	asg, err := c.getAutoScalingGroup(ctx, AutoScalingGroupNameFor(clusterName, dataplaneName))
	if err != nil {
		return err
	}
	// ASG is already gone
	if asg == nil {
		return nil
	}
	if _, err := c.autoscaling.DeleteAutoScalingGroupWithContext(ctx, &autoscaling.DeleteAutoScalingGroupInput{
		AutoScalingGroupName: ptr.String(AutoScalingGroupNameFor(clusterName, dataplaneName)),
		ForceDelete:          ptr.Bool(true), // terminate all the nodes in the ASG
	}); err != nil {
		return fmt.Errorf("deleting auto scaling group, %w", err)
	}
	return nil
}

func (c *Controller) updateAutoScalingGroup(ctx context.Context, name string, spec *v1alpha1.DataPlaneSpec, subnets []string, asg *autoscaling.Group) error {
	subnets, err := c.subnetsFor(ctx, spec)
	if err != nil {
		return fmt.Errorf("getting subnet for %s, %w", spec.ClusterName, err)
	}
	if len(subnets) == 0 {
		return fmt.Errorf("failed to find subnets for dataplane")
	}
	if functional.ValidateAll(
		func() bool { return asg != nil },
		func() bool {
			return functional.StringsMatch(strings.Split(ptr.StringValue(asg.VPCZoneIdentifier), ","), subnets)
		},
		func() bool { return ptr.Int64Value(asg.DesiredCapacity) == int64(spec.NodeCount) },
		func() bool {
			return functional.StringsMatch(
				parseOverridesFromASG(asg.MixedInstancesPolicy.LaunchTemplate.Overrides),
				parseOverridesFromASG(instanceTypes(spec.InstanceTypes)),
			)
		}) {
		return nil
	}
	zap.S().Infof("[%v] updating ASG %v", spec.ClusterName, *asg.AutoScalingGroupName)
	_, err = c.autoscaling.UpdateAutoScalingGroupWithContext(ctx, &autoscaling.UpdateAutoScalingGroupInput{
		AutoScalingGroupName: ptr.String(AutoScalingGroupNameFor(spec.ClusterName, name)),
		DesiredCapacity:      ptr.Int64(int64(spec.NodeCount)),
		VPCZoneIdentifier:    ptr.String(strings.Join(subnets, ",")),
		MixedInstancesPolicy: &autoscaling.MixedInstancesPolicy{
			LaunchTemplate: &autoscaling.LaunchTemplate{
				Overrides: instanceTypes(spec.InstanceTypes),
			},
		},
	})
	return err
}

func (c *Controller) createAutoScalingGroup(ctx context.Context, name string, spec *v1alpha1.DataPlaneSpec, subnets []string) error {
	if len(subnets) == 0 {
		return fmt.Errorf("failed to find subnets for dataplane %s", name)
	}
	_, err := c.autoscaling.CreateAutoScalingGroupWithContext(ctx, &autoscaling.CreateAutoScalingGroupInput{
		AutoScalingGroupName: ptr.String(AutoScalingGroupNameFor(spec.ClusterName, name)),
		DesiredCapacity:      ptr.Int64(int64(spec.NodeCount)),
		MaxSize:              ptr.Int64(int64(1000)),
		MinSize:              ptr.Int64(int64(0)),
		MixedInstancesPolicy: &autoscaling.MixedInstancesPolicy{
			InstancesDistribution: &autoscaling.InstancesDistribution{
				OnDemandAllocationStrategy: ptr.String(spec.AllocationStrategy),
			},
			LaunchTemplate: &autoscaling.LaunchTemplate{
				LaunchTemplateSpecification: &autoscaling.LaunchTemplateSpecification{
					LaunchTemplateName: ptr.String(launchtemplate.TemplateName(spec.ClusterName)),
				},
				Overrides: instanceTypes(spec.InstanceTypes),
			},
		},
		VPCZoneIdentifier: ptr.String(strings.Join(subnets, ",")),
		Tags:              generateAutoScalingTags(spec.ClusterName),
	})
	return err
}

func (c *Controller) getAutoScalingGroup(ctx context.Context, groupName string) (*autoscaling.Group, error) {
	output, err := c.autoscaling.DescribeAutoScalingGroupsWithContext(ctx, &autoscaling.DescribeAutoScalingGroupsInput{
		AutoScalingGroupNames: aws.StringSlice([]string{groupName}),
	})
	if err != nil {
		return nil, fmt.Errorf("getting autoscaling group, %w", err)
	}
	if len(output.AutoScalingGroups) == 0 {
		return nil, nil
	}
	if len(output.AutoScalingGroups) > 1 {
		return nil, fmt.Errorf("expected asg count one found asgs %d", len(output.AutoScalingGroups))
	}
	return output.AutoScalingGroups[0], nil
}

func (c *Controller) subnetsFor(ctx context.Context, spec *v1alpha1.DataPlaneSpec) ([]string, error) {
	// Discover subnets provided as part of the subnetSelector in DP spec.
	if len(spec.SubnetSelector) != 0 {
		return c.subnetsForSelector(ctx, spec.SubnetSelector)
	}
	// If subnetSelector is not provided fallback on control plane instance subnets
	instanceIDs, err := c.instances.ControlPlaneInstancesFor(ctx, spec.ClusterName)
	if err != nil {
		return nil, err
	}
	subnetIDs, err := c.subnetsForInstances(ctx, instanceIDs)
	if err != nil {
		return nil, fmt.Errorf("getting subnet for %s, %w", spec.ClusterName, err)
	}
	if len(subnetIDs) == 0 {
		return []string{}, nil
	}
	// Returns public subnets if no private subnets are found
	return c.filterSubnets(ctx, subnetIDs)
}

func (c *Controller) filterSubnets(ctx context.Context, ids []*string) ([]string, error) {
	output, err := c.ec2api.DescribeSubnetsWithContext(ctx, &ec2.DescribeSubnetsInput{
		SubnetIds: ids,
	})
	if err != nil {
		return nil, fmt.Errorf("describing subnet, %w", err)
	}
	public := []string{}
	private := []string{}
	for _, subnet := range output.Subnets {
		if ptr.Int64Value(subnet.AvailableIpAddressCount) == 0 {
			continue
		}
		if ptr.BoolValue(subnet.MapPublicIpOnLaunch) {
			public = append(public, *subnet.SubnetId)
		} else {
			private = append(private, *subnet.SubnetId)
		}
	}
	if len(private) > 0 {
		return private, nil
	}
	return public, nil
}

func (c *Controller) subnetsForSelector(ctx context.Context, selector map[string]string) ([]string, error) {
	filters := []*ec2.Filter{}
	// Filter by selector
	for key, value := range selector {
		if value == "*" {
			filters = append(filters, &ec2.Filter{
				Name:   aws.String("tag-key"),
				Values: []*string{aws.String(key)},
			})
		} else {
			filters = append(filters, &ec2.Filter{
				Name:   aws.String(fmt.Sprintf("tag:%s", key)),
				Values: []*string{aws.String(value)},
			})
		}
	}
	output, err := c.ec2api.DescribeSubnetsWithContext(ctx, &ec2.DescribeSubnetsInput{Filters: filters})
	if err != nil {
		return nil, fmt.Errorf("describing subnets %+v, %w", filters, err)
	}
	result := []string{}
	for _, o := range output.Subnets {
		result = append(result, *o.SubnetId)
	}
	return result, nil
}

func (c *Controller) subnetsForInstances(ctx context.Context, instanceIDs []string) ([]*string, error) {
	requestIds := []*string{}
	for _, instanceID := range instanceIDs {
		requestIds = append(requestIds, ptr.String(instanceID))
	}
	output, err := c.ec2api.DescribeInstancesWithContext(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: requestIds,
	})
	if err != nil {
		return nil, fmt.Errorf("describing ec2 instance ids, %w", err)
	}
	temp := map[*string]struct{}{}
	result := []*string{}
	for _, reservation := range output.Reservations {
		for _, instance := range reservation.Instances {
			if _, ok := temp[instance.SubnetId]; !ok {
				result = append(result, instance.SubnetId)
				temp[instance.SubnetId] = struct{}{}
			}
		}
	}
	return result, nil
}

func AutoScalingGroupNameFor(clusterName string, dataplaneName string) string {
	return fmt.Sprintf("kit/%s-cluster/%s", clusterName, dataplaneName)
}

func generateAutoScalingTags(clusterName string) []*autoscaling.Tag {
	return []*autoscaling.Tag{{
		Key:               ptr.String(fmt.Sprintf("kubernetes.io/cluster/%s", clusterName)),
		Value:             ptr.String("owned"),
		PropagateAtLaunch: aws.Bool(true),
	}, {
		Key:               aws.String("Name"),
		Value:             aws.String(fmt.Sprintf("%s-dataplane-nodes", clusterName)),
		PropagateAtLaunch: aws.Bool(true),
	}}
}

func instanceTypes(overrides []string) []*autoscaling.LaunchTemplateOverrides {
	result := []*autoscaling.LaunchTemplateOverrides{}
	for _, override := range overrides {
		result = append(result, &autoscaling.LaunchTemplateOverrides{InstanceType: ptr.String(override)})
	}
	return result
}

func parseOverridesFromASG(overrides []*autoscaling.LaunchTemplateOverrides) []string {
	result := []string{}
	for _, override := range overrides {
		result = append(result, ptr.StringValue(override.InstanceType))
	}
	return result
}
