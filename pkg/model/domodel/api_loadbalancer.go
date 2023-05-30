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

package domodel

import (
	"fmt"

	"k8s.io/kops/pkg/apis/kops"
	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kops/upup/pkg/fi/cloudup/do"
	"k8s.io/kops/upup/pkg/fi/cloudup/dotasks"
)

// APILoadBalancerModelBuilder builds a LoadBalancer for accessing the API
type APILoadBalancerModelBuilder struct {
	*DOModelContext
	Lifecycle fi.Lifecycle
}

var _ fi.CloudupModelBuilder = &APILoadBalancerModelBuilder{}

func (b *APILoadBalancerModelBuilder) Build(c *fi.CloudupModelBuilderContext) error {
	// Configuration where a load balancer fronts the API
	if !b.UseLoadBalancerForAPI() {
		return nil
	}

	lbSpec := b.Cluster.Spec.API.LoadBalancer
	if lbSpec == nil {
		// Skipping API LB creation; not requested in Spec
		return nil
	}

	switch lbSpec.Type {
	case kops.LoadBalancerTypeInternal:
		// OK
	case kops.LoadBalancerTypePublic:
		// OK
	default:
		return fmt.Errorf("unhandled LoadBalancer type %q", lbSpec.Type)
	}

	clusterName := do.SafeClusterName(b.ClusterName())
	loadbalancerName := "api-" + clusterName
	clusterMasterTag := do.TagKubernetesClusterMasterPrefix + ":" + clusterName

	// Create LoadBalancer for API LB
	loadbalancer := &dotasks.LoadBalancer{
		Name:       fi.PtrTo(loadbalancerName),
		Region:     fi.PtrTo(b.Cluster.Spec.Networking.Subnets[0].Region),
		DropletTag: fi.PtrTo(clusterMasterTag),
		Lifecycle:  b.Lifecycle,
	}

	if b.Cluster.Spec.Networking.NetworkID != "" {
		loadbalancer.VPCUUID = fi.PtrTo(b.Cluster.Spec.Networking.NetworkID)
	} else if b.Cluster.Spec.Networking.NetworkCIDR != "" {
		vpcName := "vpc-" + clusterName
		loadbalancer.VPCName = fi.PtrTo(vpcName)
		loadbalancer.NetworkCIDR = fi.PtrTo(b.Cluster.Spec.Networking.NetworkCIDR)
	}

	c.AddTask(loadbalancer)

	// Ensure the LB hostname is included in the TLS certificate,
	// if we're not going to use an alias for it
	if b.Cluster.UsesLegacyGossip() || b.Cluster.UsesPrivateDNS() || b.Cluster.UsesNoneDNS() {
		loadbalancer.ForAPIServer = true
	}

	return nil
}
