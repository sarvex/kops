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

package components

import (
	"fmt"

	"github.com/blang/semver/v4"
	"github.com/pelletier/go-toml"
	"k8s.io/kops/pkg/apis/kops"
	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kops/upup/pkg/fi/loader"
)

// ContainerdOptionsBuilder adds options for containerd to the model
type ContainerdOptionsBuilder struct {
	*OptionsContext
}

var _ loader.OptionsBuilder = &ContainerdOptionsBuilder{}

// BuildOptions is responsible for filling in the default setting for containerd daemon
func (b *ContainerdOptionsBuilder) BuildOptions(o interface{}) error {
	clusterSpec := o.(*kops.ClusterSpec)

	if clusterSpec.Containerd == nil {
		clusterSpec.Containerd = &kops.ContainerdConfig{}
	}

	containerd := clusterSpec.Containerd

	if clusterSpec.ContainerRuntime == "containerd" {
		// Set version based on Kubernetes version
		if fi.ValueOf(containerd.Version) == "" {
			switch {
			case b.IsKubernetesLT("1.23"):
				containerd.Version = fi.PtrTo("1.4.13")
			case b.IsKubernetesGTE("1.23") && b.IsKubernetesLT("1.24.14"):
				fallthrough
			case b.IsKubernetesGTE("1.25") && b.IsKubernetesLT("1.25.10"):
				fallthrough
			case b.IsKubernetesGTE("1.26") && b.IsKubernetesLT("1.26.5"):
				fallthrough
			case b.IsKubernetesGTE("1.27") && b.IsKubernetesLT("1.27.2"):
				containerd.Version = fi.PtrTo("1.6.20")
				containerd.Runc = &kops.Runc{
					Version: fi.PtrTo("1.1.5"),
				}
			default:
				containerd.Version = fi.PtrTo("1.6.21")
				containerd.Runc = &kops.Runc{
					Version: fi.PtrTo("1.1.7"),
				}
			}
		}
		// Set default log level to INFO
		containerd.LogLevel = fi.PtrTo("info")

	} else if clusterSpec.ContainerRuntime == "docker" {
		// Docker version should always be available
		dockerVersion := fi.ValueOf(clusterSpec.Docker.Version)
		if dockerVersion == "" {
			return fmt.Errorf("docker version is required")
		} else {
			// Skip containerd setup for older versions without containerd service
			sv, err := semver.ParseTolerant(dockerVersion)
			if err != nil {
				return fmt.Errorf("unable to parse version string: %q", dockerVersion)
			}
			if sv.LT(semver.MustParse("18.9.0")) {
				containerd.SkipInstall = true
				return nil
			}
		}
		// Set default log level to INFO
		containerd.LogLevel = fi.PtrTo("info")
		// Build config file for containerd running in Docker mode
		config, _ := toml.Load("")
		config.SetPath([]string{"disabled_plugins"}, []string{"cri"})
		containerd.ConfigOverride = fi.PtrTo(config.String())

	} else {
		// Unknown container runtime, should not install containerd
		containerd.SkipInstall = true
	}

	if containerd.NvidiaGPU != nil && fi.ValueOf(containerd.NvidiaGPU.Enabled) && containerd.NvidiaGPU.DriverPackage == "" {
		containerd.NvidiaGPU.DriverPackage = kops.NvidiaDefaultDriverPackage
	}

	return nil
}
