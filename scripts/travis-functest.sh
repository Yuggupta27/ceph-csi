#!/bin/bash
set -e

# This script will be used by travis to run functional test
# against different kuberentes version
export KUBE_VERSION=$1

# parse the kubernetes version, return the digit passed as argument
# v1.17.0 -> kube_version 1 -> 1
# v1.17.0 -> kube_version 2 -> 17
kube_version() {
    echo "${KUBE_VERSION}" | sed 's/^v//' | cut -d'.' -f"${1}"
}

# when running as root, "sudo" is not needed and will break the environment
SUDO='sudo'
if [[ "$(id -u)" == "0" ]]; then
       SUDO=''
fi

${SUDO} scripts/minikube.sh up
${SUDO} scripts/minikube.sh deploy-rook
${SUDO} scripts/minikube.sh create-block-pool
# pull docker images to speed up e2e
${SUDO} scripts/minikube.sh cephcsi
${SUDO} scripts/minikube.sh k8s-sidecar

# in case we run as non-root, give the user permissions to run kubectl
if [[ -n "${SUDO}" ]]; then
       ${SUDO} chown -R "$(id -u)": "$HOME"/.minikube /usr/local/bin/kubectl
fi

KUBE_MAJOR=$(kube_version 1)
KUBE_MINOR=$(kube_version 2)
# skip snapshot operation if kube version is less than 1.17.0
if [[ "${KUBE_MAJOR}" -ge 1 ]] && [[ "${KUBE_MINOR}" -ge 17 ]]; then
    # delete snapshot CRD created by ceph-csi in rook
    scripts/install-snapshot.sh delete-crd
    # install snapshot controller
    scripts/install-snapshot.sh install
fi

# functional tests
if [[ -e e2e.test ]]; then
       # e2e.test has references to the relative ../deploy/ files, it needs to
       # run from within the e2e/ directory
       pushd e2e
       ../e2e.test --deploy-timeout=10 -test.timeout=30m --cephcsi-namespace=cephcsi-e2e-$RANDOM -test.v
       popd
else
       go test github.com/ceph/ceph-csi/e2e --deploy-timeout=10 -timeout=30m --cephcsi-namespace=cephcsi-e2e-$RANDOM -v -mod=vendor
fi

if [[ "${KUBE_MAJOR}" -ge 1 ]] && [[ "${KUBE_MINOR}" -ge 17 ]]; then
    # delete snapshot CRD
    scripts/install-snapshot.sh cleanup
fi
${SUDO} scripts/minikube.sh clean
