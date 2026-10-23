#!/usr/bin/env bash
# Register a hypervisor: its own ServiceAccount, its Lease, narrow RBAC, and a
# kubeconfig for the agent. Run with cluster-admin kubectl.
#   hack/add-hypervisor.sh <hypervisor-name> <api-server-url> > kubeconfig
set -euo pipefail
HV=${1:?hypervisor name}; API=${2:?api server url, e.g. https://212.32.253.17:6443}
NS=anchorvm-system SA=agent-$HV
kubectl apply -f - >&2 <<YAML
apiVersion: v1
kind: ServiceAccount
metadata: {name: $SA, namespace: $NS}
---
apiVersion: v1
kind: Secret
metadata:
  name: $SA-token
  namespace: $NS
  annotations: {kubernetes.io/service-account.name: $SA}
type: kubernetes.io/service-account-token
---
apiVersion: coordination.k8s.io/v1
kind: Lease
metadata: {name: $SA, namespace: $NS}
spec: {holderIdentity: $HV, leaseDurationSeconds: 40}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: $SA-lease, namespace: $NS}
rules:
- apiGroups: [coordination.k8s.io]
  resources: [leases]
  resourceNames: [$SA]
  verbs: [get, update]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: $SA-lease, namespace: $NS}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: $SA-lease}
subjects: [{kind: ServiceAccount, name: $SA, namespace: $NS}]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: anchorvm-$SA}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: anchorvm-agent}
subjects: [{kind: ServiceAccount, name: $SA, namespace: $NS}]
YAML
for _ in $(seq 1 20); do
  TOKEN=$(kubectl -n $NS get secret $SA-token -o jsonpath='{.data.token}' 2>/dev/null | base64 -d)
  [ -n "$TOKEN" ] && break; sleep 1
done
CA=$(kubectl -n $NS get secret $SA-token -o jsonpath='{.data.ca\.crt}')
cat <<KC
apiVersion: v1
kind: Config
clusters: [{name: cluster, cluster: {server: "$API", certificate-authority-data: "$CA"}}]
users: [{name: $SA, user: {token: "$TOKEN"}}]
contexts: [{name: $SA, context: {cluster: cluster, user: $SA}}]
current-context: $SA
KC
