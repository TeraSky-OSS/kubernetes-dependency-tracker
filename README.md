# kubernetes-dependency-tracker
This is a simple POC application using Kubernetes informers to generate a simple API which exposes dependent resources of a provided resource generated via owner references. 
This is built in purpose of a new Backstage plugin we are releasing for visualizing all Kubernetes resources

# Simple Install
You can use helm to easily install this app
1. Create values.yaml
```yaml
ingress:
  enabled: true
  hosts:
    - host: k8s-dependency-tracker.example.com
```
2. Install the chart
```bash
helm upgrade --install deps-tracker -n dependency-tracker --create-namespace  oci://ghcr.io/terasky-oss/kubernetes-dependecy-tracker:0.1.0 -f values.yaml
```
3. Test the application
```bash
export INGRESS_HOST=<FILL ME IN>
export SA_TOKEN=<FILL ME IN>
export RESOURCE_KIND=deployment
export RESOURCE_NAMESPACE=dependency-tracker
export RESOURCE_NAME=deps-tracker-kubernetes-dependecy-tracker
export RESOURCE_API_VERSION="apps/v1"

curl "${INGRESS_HOST}/dependency?kind=${RESOURCE_KIND}&namespace=${RESOURCE_NAMESPACE}&name=${RESOURCE_NAME}&apiVersion=${RESOURCE_API_VERSION}" -H "Authorization: Bearer ${SA_TOKEN}"
```
