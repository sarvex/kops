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

package openstack

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"testing"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/openstack/loadbalancer/v2/loadbalancers"
	l3floatingips "github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/layer3/floatingips"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kops/pkg/apis/kops"
	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kops/util/pkg/vfs"
)

func Test_OpenstackCloud_MakeCloud(t *testing.T) {
	baseCloudConfigWithBlockStorage := []string{
		"auth-url=\"\"",
		"username=\"\"",
		"password=\"\"",
		"region=\"\"",
		"tenant-id=\"\"",
		"tenant-name=\"\"",
		"domain-name=\"\"",
		"domain-id=\"\"",
		"",
		"[BlockStorage]",
		"bs-version=",
		"ignore-volume-az=false",
	}

	tests := []struct {
		desc                string
		cluster             *kops.Cluster
		expectedCloudConfig []string
	}{
		{
			desc: "Ignore volume microversion is set to false when not configured",
			cluster: &kops.Cluster{
				Spec: kops.ClusterSpec{
					CloudProvider: kops.CloudProviderSpec{
						Openstack: &kops.OpenstackSpec{
							BlockStorage: &kops.OpenstackBlockStorageConfig{},
						},
					},
				},
			},
			expectedCloudConfig: append(baseCloudConfigWithBlockStorage,
				"ignore-volume-microversion=false",
				"",
			),
		},
		{
			desc: "Ignore volume microversion is set to configured value",
			cluster: &kops.Cluster{
				Spec: kops.ClusterSpec{
					CloudProvider: kops.CloudProviderSpec{
						Openstack: &kops.OpenstackSpec{
							BlockStorage: &kops.OpenstackBlockStorageConfig{
								IgnoreVolumeMicroVersion: fi.PtrTo(true),
							},
						},
					},
				},
			},
			expectedCloudConfig: append(baseCloudConfigWithBlockStorage,
				"ignore-volume-microversion=true",
				"",
			),
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.desc, func(t *testing.T) {
			actualCloudConfig := MakeCloudConfig(testCase.cluster.Spec)

			if !reflect.DeepEqual(actualCloudConfig, testCase.expectedCloudConfig) {
				t.Errorf("Ingress status differ: expected\n%+#v\n\tgot:\n%+#v\n", testCase.expectedCloudConfig, actualCloudConfig)
			}
		})
	}
}

func Test_OpenstackCloud_GetApiIngressStatus(t *testing.T) {
	tests := []struct {
		desc                 string
		cluster              *kops.Cluster
		loadbalancers        []loadbalancers.LoadBalancer
		l3FloatingIPs        []l3floatingips.FloatingIP
		instances            serverList
		cloudFloatingEnabled bool
		expectedAPIIngress   []fi.ApiIngressStatus
		expectedError        error
	}{
		{
			desc: "Loadbalancer configured master public name set",
			cluster: &kops.Cluster{
				Spec: kops.ClusterSpec{
					API: kops.APISpec{
						PublicName: "master.k8s.local",
					},
					CloudProvider: kops.CloudProviderSpec{
						Openstack: &kops.OpenstackSpec{
							Loadbalancer: &kops.OpenstackLoadbalancerConfig{},
						},
					},
				},
			},
			loadbalancers: []loadbalancers.LoadBalancer{
				{
					ID:           "lb_id",
					Name:         "name",
					VipAddress:   "10.1.2.3",
					VipPortID:    "vip_port_id",
					VipSubnetID:  "vip_subnet_id",
					VipNetworkID: "vip_network_id",
				},
			},
			l3FloatingIPs: []l3floatingips.FloatingIP{
				{
					ID:         "id",
					FixedIP:    "10.1.2.3",
					PortID:     "vip_port_id",
					FloatingIP: "8.8.8.8",
				},
			},
			expectedAPIIngress: []fi.ApiIngressStatus{
				{
					IP: "8.8.8.8",
				},
			},
		},
		{
			desc: "Loadbalancer configured master public name set multiple IPs match",
			cluster: &kops.Cluster{
				Spec: kops.ClusterSpec{
					API: kops.APISpec{
						PublicName: "master.k8s.local",
					},
					CloudProvider: kops.CloudProviderSpec{
						Openstack: &kops.OpenstackSpec{
							Loadbalancer: &kops.OpenstackLoadbalancerConfig{},
						},
					},
				},
			},
			loadbalancers: []loadbalancers.LoadBalancer{
				{
					ID:           "lb_id",
					Name:         "master.k8s.local",
					VipAddress:   "10.1.2.3",
					VipPortID:    "vip_port_id",
					VipSubnetID:  "vip_subnet_id",
					VipNetworkID: "vip_network_id",
				},
			},
			l3FloatingIPs: []l3floatingips.FloatingIP{
				{
					ID:         "cluster",
					FixedIP:    "10.1.2.3",
					PortID:     "vip_port_id",
					FloatingIP: "8.8.8.8",
				},
				{
					ID:         "something_else",
					FixedIP:    "192.168.2.3",
					PortID:     "xx_id",
					FloatingIP: "2.2.2.2",
				},
				{
					ID:         "yet_another",
					FixedIP:    "10.1.2.3",
					PortID:     "yy_id",
					FloatingIP: "9.9.9.9",
				},
			},
			expectedAPIIngress: []fi.ApiIngressStatus{
				{IP: "8.8.8.8"},
			},
		},
		{
			desc: "Loadbalancer configured master public name not set",
			cluster: &kops.Cluster{
				Spec: kops.ClusterSpec{
					CloudProvider: kops.CloudProviderSpec{
						Openstack: &kops.OpenstackSpec{
							Loadbalancer: &kops.OpenstackLoadbalancerConfig{},
						},
					},
				},
			},
			expectedAPIIngress: nil,
		},
		{
			desc: "No Loadbalancer configured no floating enabled",
			cluster: &kops.Cluster{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster.k8s.local",
				},
				Spec: kops.ClusterSpec{
					CloudProvider: kops.CloudProviderSpec{
						Openstack: &kops.OpenstackSpec{},
					},
				},
			},
			instances: []servers.Server{
				{
					ID: "master1_no_lb_no_floating",
					Metadata: map[string]string{
						"k8s":      "cluster.k8s.local",
						"KopsRole": "ControlPlane",
					},
					Addresses: map[string]interface{}{
						"1": []Address{
							{Addr: "1.2.3.4"},
							{Addr: "2.3.4.5"},
						},
						"2": []Address{
							{Addr: "3.4.5.6"},
							{Addr: "4.5.6.7"},
						},
					},
				},
				{
					ID: "master2_no_lb_no_floating",
					Metadata: map[string]string{
						"k8s":      "cluster.k8s.local",
						"KopsRole": "ControlPlane",
					},
					Addresses: map[string]interface{}{
						"1": []Address{
							{Addr: "10.20.30.40"},
							{Addr: "20.30.40.50"},
						},
						"2": []Address{
							{Addr: "30.40.50.60"},
							{Addr: "40.50.60.70"},
						},
					},
				},
				{
					ID: "node_no_lb_no_floating",
					Metadata: map[string]string{
						"k8s":      "cluster.k8s.local",
						"KopsRole": "Node",
					},
					Addresses: map[string]interface{}{
						"1": []Address{
							{Addr: "110.120.130.140", IPType: "floating"},
							{Addr: "120.130.140.150", IPType: "private"},
						},
					},
				},
			},
			expectedAPIIngress: []fi.ApiIngressStatus{
				{IP: "1.2.3.4"},
				{IP: "2.3.4.5"},
				{IP: "3.4.5.6"},
				{IP: "4.5.6.7"},
				{IP: "10.20.30.40"},
				{IP: "20.30.40.50"},
				{IP: "30.40.50.60"},
				{IP: "40.50.60.70"},
			},
		},
		{
			desc: "No Loadbalancer configured with floating enabled",
			cluster: &kops.Cluster{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster.k8s.local",
				},
				Spec: kops.ClusterSpec{
					CloudProvider: kops.CloudProviderSpec{
						Openstack: &kops.OpenstackSpec{},
					},
				},
			},
			instances: []servers.Server{
				{
					ID: "master1_no_lb_floating",
					Metadata: map[string]string{
						"k8s":      "cluster.k8s.local",
						"KopsRole": "ControlPlane",
					},
					Addresses: map[string]interface{}{
						"1": []map[string]interface{}{
							{"Addr": "1.2.3.4", "OS-EXT-IPS:type": "floating"},
							{"Addr": "2.3.4.5", "OS-EXT-IPS:type": "private"},
						},
						"2": []map[string]string{
							{"Addr": "3.4.5.6", "OS-EXT-IPS:type": "private"},
							{"Addr": "4.5.6.7", "OS-EXT-IPS:type": "floating"},
						},
					},
				},
				{
					ID: "master2_no_lb_floating",
					Metadata: map[string]string{
						"k8s":      "cluster.k8s.local",
						"KopsRole": "ControlPlane",
					},
					Addresses: map[string]interface{}{
						"1": []map[string]string{
							{"Addr": "10.20.30.40", "OS-EXT-IPS:type": "private"},
							{"Addr": "20.30.40.50", "OS-EXT-IPS:type": "private"},
						},
						"2": []map[string]string{
							{"Addr": "30.40.50.60", "OS-EXT-IPS:type": "private"},
							{"Addr": "40.50.60.70", "OS-EXT-IPS:type": "floating"},
						},
					},
				},
				{
					ID: "node_no_lb_floating",
					Metadata: map[string]string{
						"k8s":      "cluster.k8s.local",
						"KopsRole": "Node",
					},
					Addresses: map[string]interface{}{
						"1": []map[string]string{
							{"Addr": "110.120.130.140", "OS-EXT-IPS:type": "floating"},
							{"Addr": "120.130.140.150", "OS-EXT-IPS:type": "private"},
						},
					},
				},
			},
			cloudFloatingEnabled: true,
			expectedAPIIngress: []fi.ApiIngressStatus{
				{IP: "1.2.3.4"},
				{IP: "4.5.6.7"},
				{IP: "40.50.60.70"},
			},
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.desc, func(t *testing.T) {
			mux := http.NewServeMux()
			fixture(
				mux,
				"/servers/detail",
				http.MethodGet,
				string(mustJSONMarshal(json.Marshal(
					struct {
						Servers []servers.Server `json:"servers"`
					}{
						Servers: testCase.instances,
					},
				))),
				http.StatusOK,
			)
			for _, server := range testCase.instances {
				fixture(
					mux,
					fmt.Sprintf("/servers/%s", server.ID),
					http.MethodGet,
					string(mustJSONMarshal(json.Marshal(
						struct {
							Server servers.Server `json:"server"`
						}{
							Server: server,
						},
					))),
					http.StatusOK,
				)
			}
			fixture(
				mux,
				"/lbaas/loadbalancers",
				http.MethodGet,
				string(mustJSONMarshal(json.Marshal(
					struct{ LoadBalancers []loadbalancers.LoadBalancer }{
						LoadBalancers: testCase.loadbalancers,
					},
				))),
				http.StatusOK,
			)
			mux.HandleFunc("/floatingips", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				params := r.URL.Query()
				portID := params.Get("port_id")
				if portID == "" {
					fmt.Fprint(w, string(mustJSONMarshal(json.Marshal(
						struct {
							FloatingIPs []l3floatingips.FloatingIP `json:"floatingips"`
						}{
							FloatingIPs: testCase.l3FloatingIPs,
						},
					))))
					return
				}
				for _, fip := range testCase.l3FloatingIPs {
					if fip.PortID == portID {
						json := string(mustJSONMarshal(json.Marshal(
							struct {
								FloatingIPs []l3floatingips.FloatingIP `json:"floatingips"`
							}{
								FloatingIPs: []l3floatingips.FloatingIP{fip},
							},
						)))
						fmt.Fprint(w, json)
						return
					}
				}
				fmt.Fprint(w, string(mustJSONMarshal(json.Marshal(
					struct {
						FloatingIPs []l3floatingips.FloatingIP `json:"floatingips"`
					}{
						FloatingIPs: []l3floatingips.FloatingIP{},
					},
				))))
			})
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("Unexpected request for `%v`", r.URL)
				http.Error(w, "Unexpected request", http.StatusInternalServerError)
			})
			testServer := httptest.NewServer(mux)
			defer testServer.Close()

			cloud := &openstackCloud{
				floatingEnabled: testCase.cloudFloatingEnabled,
				lbClient:        serviceClient(testServer.URL),
				novaClient:      serviceClient(testServer.URL),
				neutronClient:   serviceClient(testServer.URL),
			}

			ingress, err := cloud.GetApiIngressStatus(testCase.cluster)

			compareErrors(t, testCase.expectedError, err)

			sortedExpected := sortByIP(testCase.expectedAPIIngress)
			sortedActual := sortByIP(ingress)

			sort.Sort(sortedExpected)
			sort.Sort(sortedActual)

			if !reflect.DeepEqual(sortedActual, sortedExpected) {
				t.Errorf("Ingress status differ: expected\n%+#v\n\tgot:\n%+#v\n", testCase.expectedAPIIngress, ingress)
			}
		})
	}
}

type sortByIP []fi.ApiIngressStatus

// Len is the number of elements in the collection.
func (s sortByIP) Len() int {
	return len(s)
}

// Less reports whether the element with
// index i should sort before the element with index j.
func (s sortByIP) Less(i int, j int) bool {
	return s[i].IP < s[j].IP
}

// Swap swaps the elements with indexes i and j.
func (s sortByIP) Swap(i int, j int) {
	s[i], s[j] = s[j], s[i]
}

type serverList []servers.Server

func (s serverList) Get(id string) *servers.Server {
	for _, server := range s {
		if server.ID == id {
			return &server
		}
	}
	return nil
}

func serviceClient(url string) *gophercloud.ServiceClient {
	return &gophercloud.ServiceClient{
		ProviderClient: &gophercloud.ProviderClient{},
		Endpoint:       url + "/",
	}
}

func fixture(mux *http.ServeMux, url string, method string, responseBody string, status int) {
	mux.HandleFunc(url, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Add("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, responseBody)
	})
}

func mustJSONMarshal(data []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return data
}

func compareErrors(t *testing.T, actual, expected error) {
	t.Helper()
	if pointersAreBothNil(t, "errors", actual, expected) {
		return
	}
	a := fmt.Sprintf("%v", actual)
	e := fmt.Sprintf("%v", expected)
	if a != e {
		t.Errorf("error differs: %+v instead of %+v", actual, expected)
	}
}

func pointersAreBothNil(t *testing.T, name string, actual, expected interface{}) bool {
	t.Helper()
	if actual == nil && expected == nil {
		return true
	}
	if !reflect.ValueOf(actual).IsValid() {
		return false
	}
	if reflect.ValueOf(actual).IsNil() && reflect.ValueOf(expected).IsNil() {
		return true
	}
	if actual == nil && expected != nil {
		t.Fatalf("%s differ: actual is nil, expected is not", name)
	}
	if actual != nil && expected == nil {
		t.Fatalf("%s differ: expected is nil, actual is not", name)
	}
	return false
}

func Test_BuildClients(t *testing.T) {
	tags := map[string]string{
		TagClusterName: "test.k8s.local",
	}
	provider := &gophercloud.ProviderClient{
		EndpointLocator: func(eo gophercloud.EndpointOpts) (string, error) { return "", nil },
	}

	grid := []struct {
		name                  string
		spec                  *kops.OpenstackSpec
		expectLB              bool
		expectedType          string
		expectFloatingEnabled bool
		expectError           bool
		expectedExtNetName    *string
	}{
		{
			name:         "Empty openstack spec means no load balancer",
			spec:         &kops.OpenstackSpec{},
			expectLB:     false,
			expectedType: "",
		},
		{
			name: "When octavia is set, but no router, an error should be returned",
			spec: &kops.OpenstackSpec{
				Loadbalancer: &kops.OpenstackLoadbalancerConfig{
					UseOctavia: fi.PtrTo(true),
				},
			},
			expectLB:     true,
			expectedType: "",
			expectError:  true,
		},
		{
			name: "When octavia is set, and there is a router, a load-balancer should be returned",
			spec: &kops.OpenstackSpec{
				Loadbalancer: &kops.OpenstackLoadbalancerConfig{
					UseOctavia: fi.PtrTo(true),
				},
				Router: &kops.OpenstackRouter{},
			},
			expectLB:              true,
			expectedType:          "load-balancer",
			expectFloatingEnabled: true,
			expectError:           false,
		},
		{
			name: "When octavia is not set, network should be returned",
			spec: &kops.OpenstackSpec{
				Loadbalancer: &kops.OpenstackLoadbalancerConfig{},
			},
			expectLB:     true,
			expectedType: "network",
			expectError:  false,
		},
		{
			name: "When router is set, but no LB, FIP support should be enabled",
			spec: &kops.OpenstackSpec{
				Router: &kops.OpenstackRouter{
					ExternalNetwork: fi.PtrTo("some-ext-net"),
				},
			},
			expectLB:              false,
			expectedType:          "",
			expectFloatingEnabled: true,
			expectedExtNetName:    fi.PtrTo("some-ext-net"),
		}}

	for _, g := range grid {

		t.Run(g.name, func(t *testing.T) {

			cloud, err := buildClients(provider, tags, g.spec, vfs.OpenstackConfig{}, "", false)
			if g.expectError {
				if err != nil {
					return
				} else {
					t.Fatalf("expected error, but got nil")
				}
			}
			if err != nil {
				t.Fatalf("failed to build cloud clients: %v", err)
			}

			lbClient := cloud.LoadBalancerClient()
			hasLB := cloud.LoadBalancerClient() != nil
			if hasLB != g.expectLB {
				t.Fatalf("did not match expectation. Expected: %v, actual: %v", g.expectLB, hasLB)
			}
			if g.expectLB {
				if lbClient.Type != g.expectedType {
					t.Fatalf("did not match expectation. Expected: %v, actual: %v", g.expectedType, lbClient.Type)
				}
			}

			actualFloatingEnabled := cloud.(*openstackCloud).floatingEnabled
			if g.expectFloatingEnabled != actualFloatingEnabled {
				t.Fatalf("did not match expectation. Expected: %v, actual: %v", g.expectFloatingEnabled, actualFloatingEnabled)
			}

			actualExtNetName := fi.ValueOf(cloud.(*openstackCloud).extNetworkName)
			expectedExtNetName := fi.ValueOf(g.expectedExtNetName)
			if expectedExtNetName != actualExtNetName {
				t.Fatalf("did not match expectation. Expected: %v, actual: %v", expectedExtNetName, actualExtNetName)
			}
		})
	}
}
