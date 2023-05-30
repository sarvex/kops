#!/usr/bin/env bash

# Copyright 2023 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -e
set -x

REPO_ROOT=$(git rev-parse --show-toplevel)
cd "${REPO_ROOT}"
cd ..
WORKSPACE=$(pwd)
cd "${WORKSPACE}/kops"

# Create bindir
BINDIR="${WORKSPACE}/bin"
export PATH="${BINDIR}:${PATH}"
mkdir -p "${BINDIR}"

# Build kubetest-2 kOps support
pushd "${WORKSPACE}/kops"
GOBIN=${BINDIR} make test-e2e-install
popd


# Setup our cleanup function; as we allocate resources we set a variable to indicate they should be cleaned up
function cleanup {
  # shellcheck disable=SC2153
  if [[ "${DELETE_CLUSTER:-}" == "true" ]]; then
      kubetest2 kops "${KUBETEST2_ARGS[@]}" --down || echo "kubetest2 down failed"
  fi
}
trap cleanup EXIT

# Default cluster name
SCRIPT_NAME=$(basename "$(dirname "$0")")
if [[ -z "${CLUSTER_NAME:-}" ]]; then
  CLUSTER_NAME="${SCRIPT_NAME}.k8s.local"
fi
echo "CLUSTER_NAME=${CLUSTER_NAME}"

if [[ -z "${K8S_VERSION:-}" ]]; then
  K8S_VERSION="$(curl -s -L https://dl.k8s.io/release/stable.txt)"
fi

# Download latest prebuilt kOps
if [[ -z "${KOPS_BASE_URL:-}" ]]; then
  KOPS_BASE_URL="$(curl -s https://storage.googleapis.com/kops-ci/bin/latest-ci-updown-green.txt)"
fi
export KOPS_BASE_URL

KOPS_BIN=${BINDIR}/kops
wget -qO "${KOPS_BIN}" "$KOPS_BASE_URL/$(go env GOOS)/$(go env GOARCH)/kops"
chmod +x "${KOPS_BIN}"

# Default cloud provider to aws
if [[ -z "${CLOUD_PROVIDER:-}" ]]; then
  CLOUD_PROVIDER="aws"
fi
echo "CLOUD_PROVIDER=${CLOUD_PROVIDER}"

# KOPS_STATE_STORE holds metadata about the clusters we create
if [[ -z "${KOPS_STATE_STORE:-}" ]]; then
  echo "Must specify KOPS_STATE_STORE"
  exit 1
fi
echo "KOPS_STATE_STORE=${KOPS_STATE_STORE}"
export KOPS_STATE_STORE


if [[ -z "${ADMIN_ACCESS:-}" ]]; then
  ADMIN_ACCESS="0.0.0.0/0" # Or use your IPv4 with /32
fi
echo "ADMIN_ACCESS=${ADMIN_ACCESS}"

create_args="--networking calico"


# Note that these arguments for kubetest2
KUBETEST2_ARGS=()
KUBETEST2_ARGS+=("-v=2")
KUBETEST2_ARGS+=("--cloud-provider=${CLOUD_PROVIDER}")
KUBETEST2_ARGS+=("--cluster-name=${CLUSTER_NAME:-}")
KUBETEST2_ARGS+=("--kops-binary-path=${KOPS_BIN}")
KUBETEST2_ARGS+=("--admin-access=${ADMIN_ACCESS:-}")
KUBETEST2_ARGS+=("--env=KOPS_FEATURE_FLAGS=${KOPS_FEATURE_FLAGS:-}")

# The caller can set DELETE_CLUSTER=false to stop us deleting the cluster
if [[ -z "${DELETE_CLUSTER:-}" ]]; then
  DELETE_CLUSTER="true"
fi

kubetest2 kops "${KUBETEST2_ARGS[@]}" \
  --up \
  --kubernetes-version="${K8S_VERSION}" \
  --create-args="${create_args}" \

kubetest2 kops "${KUBETEST2_ARGS[@]}" \
  --test=kops \
  --kubernetes-version="${K8S_VERSION}" \
  -- \
  --test-package-version="${K8S_VERSION}" \
  --parallel=30 \
  --skip-regex="\[Serial\]" \
  --focus-regex="\[Conformance\]"

if [[ "${DELETE_CLUSTER:-}" == "true" ]]; then
  kubetest2 kops "${KUBETEST2_ARGS[@]}" --down
  DELETE_CLUSTER=false # Don't delete again in trap
fi
