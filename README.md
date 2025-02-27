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