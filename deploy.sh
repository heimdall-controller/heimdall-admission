#!/usr/bin/env bash

set -euo pipefail

basedir="$(dirname "$0")/deployment"
keydir="$(mktemp -d)"

# Generate keys into a temporary directory.
echo "Generating TLS keys ..."
"${basedir}/generate-keys.sh" "$keydir"

# Create the `heimdall` namespace. This cannot be part of the YAML file as we first need to create the TLS secret,
# which would fail otherwise.
echo "Creating Kubernetes objects ..."
kubectl create namespace heimdall

# Create the TLS secret for the generated keys.
kubectl -n heimdall create secret tls heimdall-admission-controller-tls \
    --cert "${keydir}/heimdall-admission-controller-tls.crt" \
    --key "${keydir}/heimdall-admission-controller-tls.key"

# Read the PEM-encoded CA certificate, base64 encode it, and replace the `${CA_PEM_B64}` placeholder in the YAML
# template with it. Then, create the Kubernetes resources.
ca_pem_b64="$(openssl base64 -A <"${keydir}/ca.crt")"
sed -e 's@${CA_PEM_B64}@'"$ca_pem_b64"'@g' <"${basedir}/deployment.yaml" \
    | kubectl create -f -

rm -rf "$keydir"

echo "The webhook server has been deployed and configured!"
