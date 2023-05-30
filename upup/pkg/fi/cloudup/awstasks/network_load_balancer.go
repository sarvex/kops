/*
Copyright 2019 The Kubernetes Authors.

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

package awstasks

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/elb"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/aws/aws-sdk-go/service/route53"
	"k8s.io/klog/v2"
	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kops/upup/pkg/fi/cloudup/awsup"
	"k8s.io/kops/upup/pkg/fi/cloudup/terraform"
	"k8s.io/kops/upup/pkg/fi/cloudup/terraformWriter"
)

// NetworkLoadBalancer manages an NLB.  We find the existing NLB using the Name tag.
var _ DNSTarget = &NetworkLoadBalancer{}

// +kops:fitask
type NetworkLoadBalancer struct {
	// We use the Name tag to find the existing NLB, because we are (more or less) unrestricted when
	// it comes to tag values, but the LoadBalancerName is length limited
	Name      *string
	Lifecycle fi.Lifecycle

	// LoadBalancerName is the name in NLB, possibly different from our name
	// (NLB is restricted as to names, so we have limited choices!)
	// We use the Name tag to find the existing NLB.
	LoadBalancerName *string
	CLBName          *string

	DNSName      *string
	HostedZoneId *string

	SubnetMappings []*SubnetMapping

	Listeners []*NetworkLoadBalancerListener

	Scheme *string

	CrossZoneLoadBalancing *bool

	IpAddressType *string

	Tags         map[string]string
	ForAPIServer bool

	Type *string

	VPC          *VPC
	TargetGroups []*TargetGroup
	AccessLog    *NetworkLoadBalancerAccessLog
}

var _ fi.CompareWithID = &NetworkLoadBalancer{}
var _ fi.CloudupTaskNormalize = &NetworkLoadBalancer{}
var _ fi.CloudupProducesDeletions = &NetworkLoadBalancer{}

func (e *NetworkLoadBalancer) CompareWithID() *string {
	return e.Name
}

type NetworkLoadBalancerListener struct {
	Port             int
	TargetGroupName  string
	SSLCertificateID string
	SSLPolicy        string
}

func (e *NetworkLoadBalancerListener) mapToAWS(targetGroups []*TargetGroup, loadBalancerArn string) (*elbv2.CreateListenerInput, error) {
	var tgARN string
	for _, tg := range targetGroups {
		if fi.ValueOf(tg.Name) == e.TargetGroupName {
			tgARN = fi.ValueOf(tg.ARN)
		}
	}
	if tgARN == "" {
		return nil, fmt.Errorf("target group not found for NLB listener %+v", e)
	}

	l := &elbv2.CreateListenerInput{
		DefaultActions: []*elbv2.Action{
			{
				TargetGroupArn: aws.String(tgARN),
				Type:           aws.String(elbv2.ActionTypeEnumForward),
			},
		},
		LoadBalancerArn: aws.String(loadBalancerArn),
		Port:            aws.Int64(int64(e.Port)),
	}

	if e.SSLCertificateID != "" {
		l.Certificates = []*elbv2.Certificate{}
		l.Certificates = append(l.Certificates, &elbv2.Certificate{
			CertificateArn: aws.String(e.SSLCertificateID),
		})
		l.Protocol = aws.String(elbv2.ProtocolEnumTls)
		if e.SSLPolicy != "" {
			l.SslPolicy = aws.String(e.SSLPolicy)
		}
	} else {
		l.Protocol = aws.String(elbv2.ProtocolEnumTcp)
	}

	return l, nil
}

var _ fi.CloudupHasDependencies = &NetworkLoadBalancerListener{}

func (e *NetworkLoadBalancerListener) GetDependencies(tasks map[string]fi.CloudupTask) []fi.CloudupTask {
	return nil
}

// OrderListenersByPort implements sort.Interface for []OrderListenersByPort, based on port number
type OrderListenersByPort []*NetworkLoadBalancerListener

func (a OrderListenersByPort) Len() int      { return len(a) }
func (a OrderListenersByPort) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a OrderListenersByPort) Less(i, j int) bool {
	return a[i].Port < a[j].Port
}

// The load balancer name 'api.renamenlbcluster.k8s.local' can only contain characters that are alphanumeric characters and hyphens(-)\n\tstatus code: 400,
func findNetworkLoadBalancerByLoadBalancerName(cloud awsup.AWSCloud, loadBalancerName string) (*elbv2.LoadBalancer, error) {
	request := &elbv2.DescribeLoadBalancersInput{
		Names: []*string{&loadBalancerName},
	}
	found, err := describeNetworkLoadBalancers(cloud, request, func(lb *elbv2.LoadBalancer) bool {
		// TODO: Filter by cluster?

		if aws.StringValue(lb.LoadBalancerName) == loadBalancerName {
			return true
		}

		klog.Warningf("Got NLB with unexpected name: %q", aws.StringValue(lb.LoadBalancerName))
		return false
	})
	if err != nil {
		if awsError, ok := err.(awserr.Error); ok {
			if awsError.Code() == "LoadBalancerNotFound" {
				return nil, nil
			}
		}

		return nil, fmt.Errorf("error listing NLBs: %v", err)
	}

	if len(found) == 0 {
		return nil, nil
	}

	if len(found) != 1 {
		return nil, fmt.Errorf("Found multiple NLBs with name %q", loadBalancerName)
	}

	return found[0], nil
}

func findNetworkLoadBalancerByAlias(cloud awsup.AWSCloud, alias *route53.AliasTarget) (*elbv2.LoadBalancer, error) {
	// TODO: Any way to avoid listing all NLBs?
	request := &elbv2.DescribeLoadBalancersInput{}

	dnsName := aws.StringValue(alias.DNSName)
	matchDnsName := strings.TrimSuffix(dnsName, ".")
	if matchDnsName == "" {
		return nil, fmt.Errorf("DNSName not set on AliasTarget")
	}

	matchHostedZoneId := aws.StringValue(alias.HostedZoneId)

	found, err := describeNetworkLoadBalancers(cloud, request, func(lb *elbv2.LoadBalancer) bool {
		// TODO: Filter by cluster?

		if matchHostedZoneId != aws.StringValue(lb.CanonicalHostedZoneId) {
			return false
		}

		lbDnsName := aws.StringValue(lb.DNSName)
		lbDnsName = strings.TrimSuffix(lbDnsName, ".")
		return lbDnsName == matchDnsName || "dualstack."+lbDnsName == matchDnsName
	})
	if err != nil {
		return nil, fmt.Errorf("error listing NLBs: %v", err)
	}

	if len(found) == 0 {
		return nil, nil
	}

	if len(found) != 1 {
		return nil, fmt.Errorf("Found multiple NLBs with DNSName %q", dnsName)
	}

	return found[0], nil
}

func describeNetworkLoadBalancers(cloud awsup.AWSCloud, request *elbv2.DescribeLoadBalancersInput, filter func(*elbv2.LoadBalancer) bool) ([]*elbv2.LoadBalancer, error) {
	var found []*elbv2.LoadBalancer
	err := cloud.ELBV2().DescribeLoadBalancersPages(request, func(p *elbv2.DescribeLoadBalancersOutput, lastPage bool) (shouldContinue bool) {
		for _, lb := range p.LoadBalancers {
			if filter(lb) {
				found = append(found, lb)
			}
		}

		return true
	})
	if err != nil {
		return nil, fmt.Errorf("error listing NLBs: %v", err)
	}

	return found, nil
}

func (e *NetworkLoadBalancer) getDNSName() *string {
	return e.DNSName
}

func (e *NetworkLoadBalancer) getHostedZoneId() *string {
	return e.HostedZoneId
}

func (e *NetworkLoadBalancer) Find(c *fi.CloudupContext) (*NetworkLoadBalancer, error) {
	cloud := c.T.Cloud.(awsup.AWSCloud)

	lb, err := cloud.FindELBV2ByNameTag(e.Tags["Name"])
	if err != nil {
		return nil, err
	}
	if lb == nil {
		return nil, nil
	}

	loadBalancerArn := lb.LoadBalancerArn

	actual := &NetworkLoadBalancer{}
	actual.Name = e.Name
	actual.CLBName = e.CLBName
	actual.LoadBalancerName = lb.LoadBalancerName
	actual.DNSName = lb.DNSName
	actual.HostedZoneId = lb.CanonicalHostedZoneId // CanonicalHostedZoneNameID
	actual.Scheme = lb.Scheme
	actual.VPC = &VPC{ID: lb.VpcId}
	actual.Type = lb.Type
	actual.IpAddressType = lb.IpAddressType

	tagMap, err := cloud.DescribeELBV2Tags([]string{*loadBalancerArn})
	if err != nil {
		return nil, err
	}
	actual.Tags = make(map[string]string)
	for _, tag := range tagMap[*loadBalancerArn] {
		if strings.HasPrefix(aws.StringValue(tag.Key), "aws:cloudformation:") {
			continue
		}
		actual.Tags[aws.StringValue(tag.Key)] = aws.StringValue(tag.Value)
	}

	for _, az := range lb.AvailabilityZones {
		sm := &SubnetMapping{
			Subnet: &Subnet{ID: az.SubnetId},
		}
		for _, a := range az.LoadBalancerAddresses {
			if a.PrivateIPv4Address != nil {
				if sm.PrivateIPv4Address != nil {
					return nil, fmt.Errorf("NLB has more then one PrivateIPv4Address, which is unexpected. This is a bug in kOps, please open a GitHub issue.")
				}
				sm.PrivateIPv4Address = a.PrivateIPv4Address
			}
			if a.AllocationId != nil {
				if sm.AllocationID != nil {
					return nil, fmt.Errorf("NLB has more then one AllocationID per subnet, which is unexpected. This is a bug in kOps, please open a GitHub issue.")
				}
				sm.AllocationID = a.AllocationId
			}
		}
		actual.SubnetMappings = append(actual.SubnetMappings, sm)
	}

	{
		request := &elbv2.DescribeListenersInput{
			LoadBalancerArn: loadBalancerArn,
		}
		response, err := cloud.ELBV2().DescribeListeners(request)
		if err != nil {
			return nil, fmt.Errorf("error querying for NLB listeners :%v", err)
		}

		actual.Listeners = []*NetworkLoadBalancerListener{}
		actual.TargetGroups = []*TargetGroup{}
		for _, l := range response.Listeners {
			actualListener := &NetworkLoadBalancerListener{}
			actualListener.Port = int(aws.Int64Value(l.Port))
			if len(l.Certificates) != 0 {
				actualListener.SSLCertificateID = aws.StringValue(l.Certificates[0].CertificateArn) // What if there is more then one certificate, can we just grab the default certificate? we don't set it as default, we only set the one.
				if l.SslPolicy != nil {
					actualListener.SSLPolicy = aws.StringValue(l.SslPolicy)
				}
			}

			// This will need to be rearranged when we recognized multiple listeners and target groups per NLB
			if len(l.DefaultActions) > 0 {
				targetGroupARN := l.DefaultActions[0].TargetGroupArn
				if targetGroupARN != nil {
					targetGroupName, err := awsup.GetTargetGroupNameFromARN(fi.ValueOf(targetGroupARN))
					if err != nil {
						return nil, err
					}
					actual.TargetGroups = append(actual.TargetGroups, &TargetGroup{ARN: targetGroupARN, Name: fi.PtrTo(targetGroupName)})

					cloud := c.T.Cloud.(awsup.AWSCloud)
					descResp, err := cloud.ELBV2().DescribeTargetGroups(&elbv2.DescribeTargetGroupsInput{
						TargetGroupArns: []*string{targetGroupARN},
					})
					if err != nil {
						return nil, fmt.Errorf("error querying for NLB listener target groups: %v", err)
					}
					if len(descResp.TargetGroups) != 1 {
						return nil, fmt.Errorf("unexpected DescribeTargetGroups response: %v", descResp)
					}

					actualListener.TargetGroupName = aws.StringValue(descResp.TargetGroups[0].TargetGroupName)
				}
			}
			actual.Listeners = append(actual.Listeners, actualListener)
		}
		sort.Stable(OrderTargetGroupsByName(actual.TargetGroups))

	}

	{
		lbAttributes, err := findNetworkLoadBalancerAttributes(cloud, aws.StringValue(loadBalancerArn))
		if err != nil {
			return nil, err
		}
		klog.V(4).Infof("NLB Load Balancer attributes: %+v", lbAttributes)

		for _, attribute := range lbAttributes {
			if attribute.Value == nil {
				continue
			}
			switch key, value := attribute.Key, attribute.Value; *key {
			case "load_balancing.cross_zone.enabled":
				b, err := strconv.ParseBool(*value)
				if err != nil {
					return nil, err
				}
				actual.CrossZoneLoadBalancing = fi.PtrTo(b)
			case "access_logs.s3.enabled":
				b, err := strconv.ParseBool(*value)
				if err != nil {
					return nil, err
				}
				if actual.AccessLog == nil {
					actual.AccessLog = &NetworkLoadBalancerAccessLog{}
				}
				actual.AccessLog.Enabled = fi.PtrTo(b)
			case "access_logs.s3.bucket":
				if actual.AccessLog == nil {
					actual.AccessLog = &NetworkLoadBalancerAccessLog{}
				}
				if fi.ValueOf(value) != "" {
					actual.AccessLog.S3BucketName = value
				}
			case "access_logs.s3.prefix":
				if actual.AccessLog == nil {
					actual.AccessLog = &NetworkLoadBalancerAccessLog{}
				}
				if fi.ValueOf(value) != "" {
					actual.AccessLog.S3BucketPrefix = value
				}
			default:
				klog.V(2).Infof("unsupported key -- ignoring, %v.\n", key)
			}
		}
	}

	// Avoid spurious mismatches
	if subnetMappingSlicesEqualIgnoreOrder(actual.SubnetMappings, e.SubnetMappings) {
		actual.SubnetMappings = e.SubnetMappings
	}
	if e.DNSName == nil {
		e.DNSName = actual.DNSName
	}
	if e.HostedZoneId == nil {
		e.HostedZoneId = actual.HostedZoneId
	}
	if e.LoadBalancerName == nil {
		e.LoadBalancerName = actual.LoadBalancerName
	}

	// An existing internal NLB can't be updated to dualstack.
	if fi.ValueOf(actual.Scheme) == elbv2.LoadBalancerSchemeEnumInternal && fi.ValueOf(actual.IpAddressType) == elbv2.IpAddressTypeIpv4 {
		e.IpAddressType = actual.IpAddressType
	}

	// We allow for the LoadBalancerName to be wrong:
	// 1. We don't want to force a rename of the NLB, because that is a destructive operation
	if fi.ValueOf(e.LoadBalancerName) != fi.ValueOf(actual.LoadBalancerName) {
		klog.V(2).Infof("Reusing existing load balancer with name: %q", aws.StringValue(actual.LoadBalancerName))
		e.LoadBalancerName = actual.LoadBalancerName
	}

	_ = actual.Normalize(c)
	actual.ForAPIServer = e.ForAPIServer
	actual.Lifecycle = e.Lifecycle

	klog.V(4).Infof("Found NLB %+v", actual)

	return actual, nil
}

var _ fi.HasAddress = &NetworkLoadBalancer{}

func (e *NetworkLoadBalancer) IsForAPIServer() bool {
	return e.ForAPIServer
}

func (e *NetworkLoadBalancer) FindAddresses(context *fi.CloudupContext) ([]string, error) {
	var addresses []string

	cloud := context.T.Cloud.(awsup.AWSCloud)
	cluster := context.T.Cluster

	{
		lb, err := cloud.FindELBV2ByNameTag(e.Tags["Name"])
		if err != nil {
			return nil, fmt.Errorf("failed to find load balancer matching %q: %w", e.Tags["Name"], err)
		}
		if lb != nil && fi.ValueOf(lb.DNSName) != "" {
			addresses = append(addresses, fi.ValueOf(lb.DNSName))
		}
	}

	if cluster.UsesNoneDNS() {
		nis, err := cloud.FindELBV2NetworkInterfacesByName(fi.ValueOf(e.VPC.ID), fi.ValueOf(e.LoadBalancerName))
		if err != nil {
			return nil, fmt.Errorf("failed to find network interfaces matching %q: %w", fi.ValueOf(e.LoadBalancerName), err)
		}
		for _, ni := range nis {
			if fi.ValueOf(ni.PrivateIpAddress) != "" {
				addresses = append(addresses, fi.ValueOf(ni.PrivateIpAddress))
			}
		}
	}

	sort.Strings(addresses)

	return addresses, nil
}

func (e *NetworkLoadBalancer) Run(c *fi.CloudupContext) error {
	return fi.CloudupDefaultDeltaRunMethod(e, c)
}

func (e *NetworkLoadBalancer) Normalize(c *fi.CloudupContext) error {
	// We need to sort our arrays consistently, so we don't get spurious changes
	sort.Stable(OrderSubnetMappingsByName(e.SubnetMappings))
	sort.Stable(OrderListenersByPort(e.Listeners))
	sort.Stable(OrderTargetGroupsByName(e.TargetGroups))

	e.IpAddressType = fi.PtrTo("dualstack")
	for _, subnet := range e.SubnetMappings {
		for _, clusterSubnet := range c.T.Cluster.Spec.Networking.Subnets {
			if clusterSubnet.Name == fi.ValueOf(subnet.Subnet.ShortName) && clusterSubnet.IPv6CIDR == "" {
				e.IpAddressType = fi.PtrTo("ipv4")
			}
		}
	}

	return nil
}

func (*NetworkLoadBalancer) CheckChanges(a, e, changes *NetworkLoadBalancer) error {
	if a == nil {
		if fi.ValueOf(e.Name) == "" {
			return fi.RequiredField("Name")
		}
		if len(e.SubnetMappings) == 0 {
			return fi.RequiredField("SubnetMappings")
		}

		if e.CrossZoneLoadBalancing != nil {
			if e.CrossZoneLoadBalancing == nil {
				return fi.RequiredField("CrossZoneLoadBalancing")
			}
		}

		if e.AccessLog != nil {
			if e.AccessLog.Enabled == nil {
				return fi.RequiredField("Accesslog.Enabled")
			}
			if *e.AccessLog.Enabled {
				if e.AccessLog.S3BucketName == nil {
					return fi.RequiredField("Accesslog.S3Bucket")
				}
			}
		}
	} else {
		if len(changes.SubnetMappings) > 0 {
			expectedSubnets := make(map[string]*string)
			for _, s := range e.SubnetMappings {
				if s.AllocationID != nil {
					expectedSubnets[*s.Subnet.ID] = s.AllocationID
				} else if s.PrivateIPv4Address != nil {
					expectedSubnets[*s.Subnet.ID] = s.PrivateIPv4Address
				} else {
					expectedSubnets[*s.Subnet.ID] = nil
				}
			}

			for _, s := range a.SubnetMappings {
				eIP, ok := expectedSubnets[*s.Subnet.ID]
				if !ok {
					return fmt.Errorf("network load balancers do not support detaching subnets")
				}
				if fi.ValueOf(eIP) != fi.ValueOf(s.PrivateIPv4Address) || fi.ValueOf(eIP) != fi.ValueOf(s.AllocationID) {
					return fmt.Errorf("network load balancers do not support modifying address settings")
				}
			}
		}
	}
	return nil
}

func (_ *NetworkLoadBalancer) RenderAWS(t *awsup.AWSAPITarget, a, e, changes *NetworkLoadBalancer) error {
	var loadBalancerName string
	var loadBalancerArn string

	if len(e.Listeners) != len(e.TargetGroups) {
		return fmt.Errorf("nlb listeners and target groups do not match: %v listeners vs %v target groups", len(e.Listeners), len(e.TargetGroups))
	}

	if a == nil {
		if e.LoadBalancerName == nil {
			return fi.RequiredField("LoadBalancerName")
		}
		for _, tg := range e.TargetGroups {
			if tg.ARN == nil {
				return fmt.Errorf("missing required target group ARN for NLB creation %v", tg)
			}
		}

		loadBalancerName = *e.LoadBalancerName

		{
			request := &elbv2.CreateLoadBalancerInput{}
			request.Name = e.LoadBalancerName
			request.Scheme = e.Scheme
			request.Type = e.Type
			request.IpAddressType = e.IpAddressType
			request.Tags = awsup.ELBv2Tags(e.Tags)

			for _, subnetMapping := range e.SubnetMappings {
				request.SubnetMappings = append(request.SubnetMappings, &elbv2.SubnetMapping{
					SubnetId:           subnetMapping.Subnet.ID,
					AllocationId:       subnetMapping.AllocationID,
					PrivateIPv4Address: subnetMapping.PrivateIPv4Address,
				})
			}

			klog.V(2).Infof("Creating NLB %q", loadBalancerName)

			response, err := t.Cloud.ELBV2().CreateLoadBalancer(request)
			if err != nil {
				return fmt.Errorf("error creating NLB %q: %w", loadBalancerName, err)
			}
			if len(response.LoadBalancers) != 1 {
				return fmt.Errorf("error creating NLB %q: found %d", loadBalancerName, len(response.LoadBalancers))
			}

			lb := response.LoadBalancers[0]
			e.DNSName = lb.DNSName
			e.HostedZoneId = lb.CanonicalHostedZoneId
			e.VPC = &VPC{ID: lb.VpcId}
			loadBalancerArn = fi.ValueOf(lb.LoadBalancerArn)

		}

		// Wait for all load balancer components to be created (including network interfaces needed for NoneDNS).
		// Limiting this to clusters using NoneDNS because load balancer creation is quite slow.
		for _, tg := range e.TargetGroups {
			if strings.HasPrefix(fi.ValueOf(tg.Name), "kops-controller") {
				klog.Infof("Waiting for load balancer %q to be created...", loadBalancerName)
				request := &elbv2.DescribeLoadBalancersInput{
					Names: []*string{&loadBalancerName},
				}
				err := t.Cloud.ELBV2().WaitUntilLoadBalancerAvailable(request)
				if err != nil {
					return fmt.Errorf("error waiting for NLB %q: %w", loadBalancerName, err)
				}
				break
			}
		}

		{
			for _, listener := range e.Listeners {
				createListenerInput, err := listener.mapToAWS(e.TargetGroups, loadBalancerArn)
				if err != nil {
					return err
				}

				klog.V(2).Infof("Creating Listener for NLB with port %v", listener.Port)
				_, err = t.Cloud.ELBV2().CreateListener(createListenerInput)
				if err != nil {
					return fmt.Errorf("error creating listener for NLB: %v", err)
				}
			}
		}
	} else {
		loadBalancerName = fi.ValueOf(a.LoadBalancerName)

		lb, err := findNetworkLoadBalancerByLoadBalancerName(t.Cloud, loadBalancerName)
		if err != nil {
			return fmt.Errorf("error getting load balancer by name: %v", err)
		}

		loadBalancerArn = fi.ValueOf(lb.LoadBalancerArn)

		if changes.IpAddressType != nil {
			request := &elbv2.SetIpAddressTypeInput{
				IpAddressType:   e.IpAddressType,
				LoadBalancerArn: lb.LoadBalancerArn,
			}
			if _, err := t.Cloud.ELBV2().SetIpAddressType(request); err != nil {
				return fmt.Errorf("error setting the IP addresses type: %v", err)
			}
		}

		if changes.SubnetMappings != nil {
			actualSubnets := make(map[string]*string)
			for _, s := range a.SubnetMappings {
				// actualSubnets[*s.Subnet.ID] = s
				if s.AllocationID != nil {
					actualSubnets[*s.Subnet.ID] = s.AllocationID
				}
				if s.PrivateIPv4Address != nil {
					actualSubnets[*s.Subnet.ID] = s.PrivateIPv4Address
				}
			}

			var awsSubnetMappings []*elbv2.SubnetMapping
			hasChanges := false
			for _, s := range e.SubnetMappings {
				aIP, ok := actualSubnets[*s.Subnet.ID]
				if !ok || (fi.ValueOf(s.PrivateIPv4Address) != fi.ValueOf(aIP) && fi.ValueOf(s.AllocationID) != fi.ValueOf(aIP)) {
					hasChanges = true
				}
				awsSubnetMappings = append(awsSubnetMappings, &elbv2.SubnetMapping{
					SubnetId:           s.Subnet.ID,
					AllocationId:       s.AllocationID,
					PrivateIPv4Address: s.PrivateIPv4Address,
				})
			}

			if hasChanges {
				request := &elbv2.SetSubnetsInput{}
				request.SetLoadBalancerArn(loadBalancerArn)
				request.SetSubnetMappings(awsSubnetMappings)

				klog.V(2).Infof("Attaching Load Balancer to new subnets")
				if _, err := t.Cloud.ELBV2().SetSubnets(request); err != nil {
					return fmt.Errorf("error attaching load balancer to new subnets: %v", err)
				}
			}
		}

		if changes.Listeners != nil {

			if lb != nil {

				request := &elbv2.DescribeListenersInput{
					LoadBalancerArn: lb.LoadBalancerArn,
				}
				response, err := t.Cloud.ELBV2().DescribeListeners(request)
				if err != nil {
					return fmt.Errorf("error querying for NLB listeners :%v", err)
				}

				for _, l := range response.Listeners {
					// delete the listener before recreating it
					_, err := t.Cloud.ELBV2().DeleteListener(&elbv2.DeleteListenerInput{
						ListenerArn: l.ListenerArn,
					})
					if err != nil {
						return fmt.Errorf("error deleting load balancer listener with arn = : %v : %v", l.ListenerArn, err)
					}
				}
			}

			for _, listener := range changes.Listeners {

				awsListener, err := listener.mapToAWS(e.TargetGroups, loadBalancerArn)
				if err != nil {
					return err
				}

				klog.V(2).Infof("Creating Listener for NLB with port %v", listener.Port)
				_, err = t.Cloud.ELBV2().CreateListener(awsListener)
				if err != nil {
					return fmt.Errorf("error creating NLB listener: %v", err)
				}
			}
		}
		if err := t.AddELBV2Tags(loadBalancerArn, e.Tags); err != nil {
			return err
		}

		if err := t.RemoveELBV2Tags(loadBalancerArn, e.Tags); err != nil {
			return err
		}
	}

	if err := e.modifyLoadBalancerAttributes(t, a, e, changes, loadBalancerArn); err != nil {
		klog.Infof("error modifying NLB attributes: %v", err)
		return err
	}
	return nil
}

type terraformNetworkLoadBalancer struct {
	Name                   string                                      `cty:"name"`
	Internal               bool                                        `cty:"internal"`
	Type                   string                                      `cty:"load_balancer_type"`
	IPAddressType          *string                                     `cty:"ip_address_type"`
	SubnetMappings         []terraformNetworkLoadBalancerSubnetMapping `cty:"subnet_mapping"`
	CrossZoneLoadBalancing bool                                        `cty:"enable_cross_zone_load_balancing"`
	AccessLog              *terraformNetworkLoadBalancerAccessLog      `cty:"access_logs"`

	Tags map[string]string `cty:"tags"`
}

type terraformNetworkLoadBalancerSubnetMapping struct {
	Subnet             *terraformWriter.Literal `cty:"subnet_id"`
	AllocationID       *string                  `cty:"allocation_id"`
	PrivateIPv4Address *string                  `cty:"private_ipv4_address"`
}

type terraformNetworkLoadBalancerListener struct {
	LoadBalancer   *terraformWriter.Literal                     `cty:"load_balancer_arn"`
	Port           int64                                        `cty:"port"`
	Protocol       string                                       `cty:"protocol"`
	CertificateARN *string                                      `cty:"certificate_arn"`
	SSLPolicy      *string                                      `cty:"ssl_policy"`
	DefaultAction  []terraformNetworkLoadBalancerListenerAction `cty:"default_action"`
}

type terraformNetworkLoadBalancerListenerAction struct {
	Type           string                   `cty:"type"`
	TargetGroupARN *terraformWriter.Literal `cty:"target_group_arn"`
}

func (_ *NetworkLoadBalancer) RenderTerraform(t *terraform.TerraformTarget, a, e, changes *NetworkLoadBalancer) error {
	nlbTF := &terraformNetworkLoadBalancer{
		Name:                   *e.LoadBalancerName,
		Internal:               fi.ValueOf(e.Scheme) == elbv2.LoadBalancerSchemeEnumInternal,
		Type:                   elbv2.LoadBalancerTypeEnumNetwork,
		Tags:                   e.Tags,
		CrossZoneLoadBalancing: fi.ValueOf(e.CrossZoneLoadBalancing),
	}
	if fi.ValueOf(e.IpAddressType) == "dualstack" {
		nlbTF.IPAddressType = e.IpAddressType
	}

	for _, subnetMapping := range e.SubnetMappings {
		nlbTF.SubnetMappings = append(nlbTF.SubnetMappings, terraformNetworkLoadBalancerSubnetMapping{
			Subnet:             subnetMapping.Subnet.TerraformLink(),
			AllocationID:       subnetMapping.AllocationID,
			PrivateIPv4Address: subnetMapping.PrivateIPv4Address,
		})
	}

	if e.AccessLog != nil && fi.ValueOf(e.AccessLog.Enabled) {
		nlbTF.AccessLog = &terraformNetworkLoadBalancerAccessLog{
			Enabled:        e.AccessLog.Enabled,
			S3BucketName:   e.AccessLog.S3BucketName,
			S3BucketPrefix: e.AccessLog.S3BucketPrefix,
		}
	}

	err := t.RenderResource("aws_lb", *e.Name, nlbTF)
	if err != nil {
		return err
	}

	for _, listener := range e.Listeners {
		var listenerTG *TargetGroup
		for _, tg := range e.TargetGroups {
			if aws.StringValue(tg.Name) == listener.TargetGroupName {
				listenerTG = tg
				break
			}
		}
		if listenerTG == nil {
			return fmt.Errorf("target group not found for NLB listener %+v", e)
		}
		listenerTF := &terraformNetworkLoadBalancerListener{
			LoadBalancer: e.TerraformLink(),
			Port:         int64(listener.Port),
			DefaultAction: []terraformNetworkLoadBalancerListenerAction{
				{
					Type:           elbv2.ActionTypeEnumForward,
					TargetGroupARN: listenerTG.TerraformLink(),
				},
			},
		}
		if listener.SSLCertificateID != "" {
			listenerTF.CertificateARN = &listener.SSLCertificateID
			listenerTF.Protocol = elbv2.ProtocolEnumTls
			if listener.SSLPolicy != "" {
				listenerTF.SSLPolicy = &listener.SSLPolicy
			}
		} else {
			listenerTF.Protocol = elbv2.ProtocolEnumTcp
		}

		err = t.RenderResource("aws_lb_listener", fmt.Sprintf("%v-%v", *e.Name, listener.Port), listenerTF)
		if err != nil {
			return err
		}
	}

	return nil
}

func (e *NetworkLoadBalancer) TerraformLink(params ...string) *terraformWriter.Literal {
	prop := "id"
	if len(params) > 0 {
		prop = params[0]
	}
	return terraformWriter.LiteralProperty("aws_lb", *e.Name, prop)
}

// FindDeletions schedules deletion of the corresponding legacy classic load balancer when it no longer has targets.
func (e *NetworkLoadBalancer) FindDeletions(context *fi.CloudupContext) ([]fi.CloudupDeletion, error) {
	if e.CLBName == nil {
		return nil, nil
	}

	cloud := context.T.Cloud.(awsup.AWSCloud)

	lb, err := cloud.FindELBByNameTag(fi.ValueOf(e.CLBName))
	if err != nil {
		return nil, err
	}
	if lb == nil {
		return nil, nil
	}

	// Testing shows that the instances are deregistered immediately after the apply_cluster.
	// TODO: Figure out how to delay deregistration until instances are terminated.
	//if len(lb.Instances) > 0 {
	//	klog.V(2).Infof("CLB %s has targets; not scheduling deletion", *lb.LoadBalancerName)
	//	return nil, nil
	//}

	actual := &deleteClassicLoadBalancer{}
	actual.LoadBalancerName = lb.LoadBalancerName

	klog.V(4).Infof("Found CLB %+v", actual)

	return []fi.CloudupDeletion{actual}, nil
}

type deleteClassicLoadBalancer struct {
	// LoadBalancerName is the name in ELB, possibly different from our name
	// (ELB is restricted as to names, so we have limited choices!)
	LoadBalancerName *string
}

func (d deleteClassicLoadBalancer) Delete(t fi.CloudupTarget) error {
	awsTarget, ok := t.(*awsup.AWSAPITarget)
	if !ok {
		return fmt.Errorf("unexpected target type for deletion: %T", t)
	}

	_, err := awsTarget.Cloud.ELB().DeleteLoadBalancer(&elb.DeleteLoadBalancerInput{
		LoadBalancerName: d.LoadBalancerName,
	})
	if err != nil {
		return fmt.Errorf("deleting classic LoadBalancer: %w", err)
	}

	return nil
}

func (d deleteClassicLoadBalancer) TaskName() string {
	return "ClassicLoadBalancer"
}

func (d deleteClassicLoadBalancer) Item() string {
	return *d.LoadBalancerName
}
