package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	//"path/filepath"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

	//"k8s.io/client-go/util/homedir"

	apiextensionsclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
)

type ResourceNode struct {
	Kind       string         `json:"kind"`
	Plural     string         `json:"plural"`
	Name       string         `json:"name"`
	Namespace  string         `json:"namespace,omitempty"`
	APIVersion string         `json:"apiVersion"`
	Dependants []ResourceNode `json:"dependants,omitempty"`
}

type DependencyGraph struct {
	sync.RWMutex
	resources      map[string]metav1.Object
	pluralMap      map[string]string
	kindMap        map[string]string
	scopeMap       map[string]bool
	storedVersions map[string]string // maps Group/Kind to stored version
}

func shouldSkipResource(apiResource metav1.APIResource) bool {
	// Skip subresources
	if strings.Contains(apiResource.Name, "/") {
		return true
	}

	// Skip resources that can't be listed/watched
	if !sets.NewString(apiResource.Verbs...).HasAll("list", "watch") {
		return true
	}

	// Skip known problematic resources
	skipResources := sets.NewString(
		"componentstatuses",
		"selfsubjectreviews",
		"selfsubjectrulesreviews",
		"selfsubjectaccessreviews",
		"tokenreviews",
		"localsubjectaccessreviews",
		"subjectaccessreviews",
	)
	return skipResources.Has(strings.ToLower(apiResource.Name))
}

// Add this function before main()
func verifyAccess(token string, resource schema.GroupVersionResource, namespace, name string, config *rest.Config) bool {
	// Create a new config with the user's token
	userConfig := rest.CopyConfig(config)
	userConfig.BearerToken = token

	// Create dynamic client with user's credentials
	userClient, err := dynamic.NewForConfig(userConfig)
	if err != nil {
		return false
	}

	// Try to get the resource
	if namespace != "" {
		_, err = userClient.Resource(resource).Namespace(namespace).Get(context.Background(), name, metav1.GetOptions{})
	} else {
		_, err = userClient.Resource(resource).Get(context.Background(), name, metav1.GetOptions{})
	}

	return err == nil
}

// Define the error handler function with the correct type signature
func handleInformerError(r *cache.Reflector, err error) {
	fmt.Printf("Informer error (non-fatal) for reflector %s: %v\n", r.Name(), err)
}

// Add this function to check if a resource is valid
func isValidResource(dynamicClient dynamic.Interface, gvr schema.GroupVersionResource) bool {
	_, err := dynamicClient.Resource(gvr).List(context.TODO(), metav1.ListOptions{Limit: 1})
	return err == nil
}

func main() {
	// Add at the beginning of main()
	var insecure bool
	flag.BoolVar(&insecure, "insecure", false, "Run without authentication")
	flag.Parse()

	// Create kubernetes config
	var config *rest.Config
	var err error

	// Try in-cluster config first, fallback to kubeconfig
	if _, err := os.Stat("/var/run/secrets/kubernetes.io/serviceaccount/token"); err == nil {
		config, _ = rest.InClusterConfig()
	} else {
		//kubeconfig := filepath.Join(homedir.HomeDir(), ".kube", "config")
		kubeconfig := os.Getenv("KUBECONFIG")

		config, _ = clientcmd.BuildConfigFromFlags("", kubeconfig)
	}

	// Increase QPS and Burst limits
	config.QPS = 100
	config.Burst = 200

	// Create discovery client with increased timeout
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		panic(err)
	}

	// Create dynamic client
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		panic(err)
	}

	// Initialize dependency graph
	graph := &DependencyGraph{
		resources:      make(map[string]metav1.Object),
		pluralMap:      make(map[string]string),
		kindMap:        make(map[string]string),
		scopeMap:       make(map[string]bool),
		storedVersions: make(map[string]string),
	}

	// Initialize stored versions
	if err := graph.initializeStoredVersions(config); err != nil {
		fmt.Printf("Warning: Failed to initialize stored versions: %v\n", err)
	}

	// Create factory with reasonable resync period
	factory := dynamicinformer.NewDynamicSharedInformerFactory(dynamicClient, time.Minute*30)

	// Get all API resources from the cluster
	_, apiResourcesList, err := discoveryClient.ServerGroupsAndResources()
	if err != nil {
		if !discovery.IsGroupDiscoveryFailedError(err) {
			panic(err)
		}
		fmt.Printf("Warning: Some groups were not discovered: %v\n", err)
	}

	// Track started informers
	startedInformers := make(map[string]bool)

	// Track successfully started informers
	successfulInformers := make(map[schema.GroupVersionResource]cache.SharedIndexInformer)

	for _, apiResources := range apiResourcesList {
		gv, err := schema.ParseGroupVersion(apiResources.GroupVersion)
		if err != nil {
			fmt.Printf("Warning: Error parsing GroupVersion %s: %v\n", apiResources.GroupVersion, err)
			continue
		}

		for _, apiResource := range apiResources.APIResources {
			// Skip if resource should not be watched
			if shouldSkipResource(apiResource) {
				continue
			}

			// Create unique key for this resource
			resourceKey := strings.ToLower(fmt.Sprintf("%s/%s/%s", gv.Group, gv.Version, apiResource.Name))

			// Skip if we've already started an informer for this resource
			if startedInformers[resourceKey] {
				continue
			}

			// Create GVR
			gvr := schema.GroupVersionResource{
				Group:    gv.Group,
				Version:  gv.Version,
				Resource: apiResource.Name,
			}

			// Check if the resource is valid
			if !isValidResource(dynamicClient, gvr) {
				fmt.Printf("Skipping invalid resource: %s\n", resourceKey)
				continue
			}

			// Store mappings (all in lowercase)
			graph.pluralMap[strings.ToLower(apiResource.Kind)] = strings.ToLower(apiResource.Name)
			graph.kindMap[strings.ToLower(resourceKey)] = strings.ToLower(apiResource.Kind)
			graph.scopeMap[strings.ToLower(apiResource.Kind)] = apiResource.Namespaced

			// Try to create and start informer
			func() {
				defer func() {
					if r := recover(); r != nil {
						fmt.Printf("Recovered from panic while creating informer for %s: %v\n", resourceKey, r)
					}
				}()

				// Inside the informer setup code, use the defined error handler function
				informer := factory.ForResource(gvr).Informer()

				// Set the watch error handler
				informer.SetWatchErrorHandler(cache.WatchErrorHandler(handleInformerError))

				gvrCopy := gvr
				informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
					AddFunc: func(obj interface{}) {
						if meta, ok := obj.(metav1.Object); ok {
							graph.Lock()
							key := buildResourceKey(gvrCopy, meta)
							graph.resources[key] = meta
							graph.Unlock()
						}
					},
					UpdateFunc: func(_, new interface{}) {
						if meta, ok := new.(metav1.Object); ok {
							graph.Lock()
							key := buildResourceKey(gvrCopy, meta)
							graph.resources[key] = meta
							graph.Unlock()
						}
					},
					DeleteFunc: func(obj interface{}) {
						if meta, ok := obj.(metav1.Object); ok {
							graph.Lock()
							key := buildResourceKey(gvrCopy, meta)
							delete(graph.resources, key)
							graph.Unlock()
						}
					},
				})

				successfulInformers[gvr] = informer
				startedInformers[resourceKey] = true
			}()
		}
	}

	// Create a context that we can cancel
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start informers
	fmt.Println("Starting informers...")
	factory.Start(ctx.Done())

	// Wait for caches to sync, but don't fail if some don't sync
	fmt.Println("Waiting for caches to sync...")
	time.Sleep(2 * time.Second) // Give some time for informers to start

	synced := factory.WaitForCacheSync(ctx.Done())
	for gvr, ok := range synced {
		if !ok {
			fmt.Printf("Warning: Cache failed to sync for %v\n", gvr)
		} else {
			fmt.Printf("Cache synced for %v\n", gvr)
		}
	}

	// Add logging for startup
	fmt.Println("Starting Kubernetes resource watcher...")

	// Set up HTTP handler with timeouts and logging
	server := &http.Server{
		Addr:         ":8080",
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			fmt.Printf("Received request: %s\n", r.URL.String())

			if r.URL.Path != "/dependency" {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}

			// Check authentication if not in insecure mode
			var userToken string
			if !insecure {
				authHeader := r.Header.Get("Authorization")
				if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
					http.Error(w, "unauthorized: missing or invalid authorization header", http.StatusUnauthorized)
					return
				}
				userToken = strings.TrimPrefix(authHeader, "Bearer ")
			}

			name := r.URL.Query().Get("name")
			namespace := r.URL.Query().Get("namespace")
			kind := r.URL.Query().Get("kind")
			apiVersion := r.URL.Query().Get("apiVersion")

			fmt.Printf("Processing request for %s/%s/%s/%s\n", apiVersion, kind, namespace, name)

			missingParams := []string{}
			if name == "" {
				missingParams = append(missingParams, "name")
			}
			if kind == "" {
				missingParams = append(missingParams, "kind")
			}
			if apiVersion == "" {
				missingParams = append(missingParams, "apiVersion")
			}

			if len(missingParams) > 0 {
				errMsg := fmt.Sprintf("missing required parameters: %s", strings.Join(missingParams, ", "))
				http.Error(w, errMsg, http.StatusBadRequest)
				return
			}

			// Create a new buildDependencyTree call with token validation
			node := graph.buildDependencyTreeWithAuth(kind, apiVersion, namespace, name, userToken, config, insecure)
			if node == nil {
				http.Error(w, "resource not found or access denied", http.StatusNotFound)
				return
			}

			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(node); err != nil {
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}

			fmt.Printf("Request completed in %v\n", time.Since(start))
		}),
	}

	// Start HTTP server
	fmt.Println("Starting HTTP server on :8080")
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		panic(fmt.Sprintf("Failed to start HTTP server: %v", err))
	}
}

func (g *DependencyGraph) buildDependencyTree(kind, apiVersion, namespace, name string) *ResourceNode {
	// Convert input parameters to lowercase for consistent comparison
	kind = strings.ToLower(kind)
	apiVersion = strings.ToLower(apiVersion)
	namespace = strings.ToLower(namespace)
	name = strings.ToLower(name)

	// Use RLock for reading, but be careful not to hold it too long
	g.RLock()
	isNamespaced := g.scopeMap[kind]
	plural := g.pluralMap[kind]
	g.RUnlock()

	if plural == "" {
		fmt.Printf("Warning: Unknown resource kind: %s\n", kind)
		return nil
	}

	// Build the key based on whether the resource is namespaced
	var key string
	group, version := "", ""
	if strings.Contains(apiVersion, "/") {
		parts := strings.Split(apiVersion, "/")
		group, version = parts[0], parts[1]
	} else {
		version = apiVersion
	}

	if isNamespaced {
		if namespace == "" {
			fmt.Printf("Warning: Namespace required for namespaced resource %s\n", kind)
			return nil
		}
		key = fmt.Sprintf("%s/%s/%s/%s/%s", group, version, plural, namespace, name)
	} else {
		key = fmt.Sprintf("%s/%s/%s/cluster/%s", group, version, plural, name)
	}

	key = strings.ToLower(key)

	// Get the initial resource
	g.RLock()
	_, exists := g.resources[key]
	g.RUnlock()

	if !exists {
		return nil
	}

	// Build the node
	node := &ResourceNode{
		Kind:       kind,
		Plural:     plural,
		Name:       name,
		Namespace:  namespace,
		APIVersion: apiVersion,
	}

	// Find dependants
	g.RLock()
	resources := make(map[string]metav1.Object)
	kindMap := make(map[string]string)
	for k, v := range g.resources {
		resources[k] = v
	}
	for k, v := range g.kindMap {
		kindMap[k] = v
	}
	g.RUnlock()

	// Process all resources to find dependants
	for resKey, other := range resources {
		ownerRefs := other.GetOwnerReferences()
		for _, ownerRef := range ownerRefs {
			// Convert owner reference values to lowercase
			ownerName := strings.ToLower(ownerRef.Name)
			ownerKind := strings.ToLower(ownerRef.Kind)
			ownerAPIVersion := strings.ToLower(ownerRef.APIVersion)

			// Check if this resource is owned by our target
			if ownerName != name || ownerKind != kind {
				continue
			}

			// Handle API version matching
			ownerAPIGroup, ownerAPIVersion := "", ownerAPIVersion
			if strings.Contains(ownerAPIVersion, "/") {
				parts := strings.Split(ownerAPIVersion, "/")
				ownerAPIGroup, ownerAPIVersion = parts[0], parts[1]
			}

			targetAPIGroup, targetAPIVersion := "", apiVersion
			if strings.Contains(apiVersion, "/") {
				parts := strings.Split(apiVersion, "/")
				targetAPIGroup, targetAPIVersion = parts[0], parts[1]
			}

			// Special handling for core API group
			versionMatch := (ownerAPIGroup == targetAPIGroup && ownerAPIVersion == targetAPIVersion) ||
				(ownerAPIGroup == "" && targetAPIGroup == "" && ownerAPIVersion == targetAPIVersion)

			if !versionMatch {
				continue
			}

			// Get the correct kind for the dependant
			parts := strings.Split(resKey, "/")
			gvr := strings.Join(parts[:3], "/")
			otherKind := kindMap[gvr]

			var otherNs string
			if len(parts) > 3 && parts[3] != "cluster" {
				otherNs = parts[3]
			}

			dependant := g.buildDependencyTree(
				otherKind,
				parts[0]+"/"+parts[1],
				otherNs,
				strings.ToLower(other.GetName()),
			)
			if dependant != nil {
				node.Dependants = append(node.Dependants, *dependant)
			}
		}
	}

	return node
}

// Add this method to the DependencyGraph struct
func (g *DependencyGraph) buildDependencyTreeWithAuth(kind, apiVersion, namespace, name, token string, config *rest.Config, insecure bool) *ResourceNode {
	// Convert input parameters to lowercase for consistent comparison
	kind = strings.ToLower(kind)
	apiVersion = strings.ToLower(apiVersion)
	namespace = strings.ToLower(namespace)
	name = strings.ToLower(name)

	g.RLock()
	isNamespaced := g.scopeMap[kind]
	plural := g.pluralMap[kind]
	g.RUnlock()

	if plural == "" {
		fmt.Printf("Warning: Unknown resource kind: %s\n", kind)
		return nil
	}

	// Parse group and version
	group, version := "", ""
	if strings.Contains(apiVersion, "/") {
		parts := strings.Split(apiVersion, "/")
		group, version = parts[0], parts[1]
	} else {
		version = apiVersion
	}

	// Build the key based on whether the resource is namespaced
	var key string
	if isNamespaced {
		if namespace == "" {
			fmt.Printf("Warning: Namespace required for namespaced resource %s\n", kind)
			return nil
		}
		key = fmt.Sprintf("%s/%s/%s/%s/%s", group, version, plural, namespace, name)
	} else {
		key = fmt.Sprintf("%s/%s/%s/cluster/%s", group, version, plural, name)
	}

	key = strings.ToLower(key)

	// Get the initial resource
	g.RLock()
	_, exists := g.resources[key]
	g.RUnlock()

	if !exists {
		return nil
	}

	// Create GVR for access check
	gvr := schema.GroupVersionResource{
		Group:    group,
		Version:  version,
		Resource: plural,
	}

	// Verify access to the requested resource if not in insecure mode
	if !insecure {
		if !verifyAccess(token, gvr, namespace, name, config) {
			return nil
		}
	}

	// Build the node
	node := &ResourceNode{
		Kind:       kind,
		Plural:     plural,
		Name:       name,
		Namespace:  namespace,
		APIVersion: apiVersion,
	}

	// Find dependants
	g.RLock()
	resources := make(map[string]metav1.Object)
	kindMap := make(map[string]string)
	for k, v := range g.resources {
		resources[k] = v
	}
	for k, v := range g.kindMap {
		kindMap[k] = v
	}
	g.RUnlock()

	// Process all resources to find dependants
	seenResources := make(map[string]bool) // Track processed resources by name

	for resKey, other := range resources {
		ownerRefs := other.GetOwnerReferences()
		for _, ownerRef := range ownerRefs {
			// Convert owner reference values to lowercase
			ownerName := strings.ToLower(ownerRef.Name)
			ownerKind := strings.ToLower(ownerRef.Kind)
			ownerAPIVersion := strings.ToLower(ownerRef.APIVersion)

			// Check if this resource is owned by our target
			if ownerName != name || ownerKind != kind {
				continue
			}

			// Handle API version matching
			ownerAPIGroup, ownerAPIVersion := "", ownerAPIVersion
			if strings.Contains(ownerAPIVersion, "/") {
				parts := strings.Split(ownerAPIVersion, "/")
				ownerAPIGroup, ownerAPIVersion = parts[0], parts[1]
			}

			targetAPIGroup, targetAPIVersion := "", apiVersion
			if strings.Contains(apiVersion, "/") {
				parts := strings.Split(apiVersion, "/")
				targetAPIGroup, targetAPIVersion = parts[0], parts[1]
			}

			// Special handling for core API group
			versionMatch := (ownerAPIGroup == targetAPIGroup && ownerAPIVersion == targetAPIVersion) ||
				(ownerAPIGroup == "" && targetAPIGroup == "" && ownerAPIVersion == targetAPIVersion)

			if !versionMatch {
				continue
			}

			// Get the correct kind for the dependant
			parts := strings.Split(resKey, "/")
			gvr := strings.Join(parts[:3], "/")
			otherKind := kindMap[gvr]

			// Skip if we've already processed this resource
			resourceIdentifier := fmt.Sprintf("%s/%s/%s", otherKind, other.GetNamespace(), other.GetName())
			if seenResources[resourceIdentifier] {
				continue
			}

			// Check if this is the stored version
			groupKindKey := fmt.Sprintf("%s/%s", parts[0], otherKind)
			storedVersion := g.storedVersions[strings.ToLower(groupKindKey)]

			// Modified version check:
			// Skip only if we have stored version info AND it doesn't match
			// This allows core resources (which have no stored version) to pass through
			if storedVersion != "" && storedVersion != parts[1] {
				continue
			}

			// For core API resources (empty group), always allow the preferred version
			if parts[0] == "" && parts[1] != "v1" {
				continue
			}

			var otherNs string
			if len(parts) > 3 && parts[3] != "cluster" {
				otherNs = parts[3]
			}

			dependant := g.buildDependencyTreeWithAuth(
				otherKind,
				parts[0]+"/"+parts[1],
				otherNs,
				strings.ToLower(other.GetName()),
				token,
				config,
				insecure,
			)
			if dependant != nil {
				node.Dependants = append(node.Dependants, *dependant)
				seenResources[resourceIdentifier] = true
			}
		}
	}

	return node
}

// Add this function to initialize the stored versions
func (g *DependencyGraph) initializeStoredVersions(config *rest.Config) error {
	g.storedVersions = make(map[string]string)

	apiextensionsClient, err := apiextensionsclientset.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("failed to create apiextensions client: %v", err)
	}

	crds, err := apiextensionsClient.ApiextensionsV1().CustomResourceDefinitions().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list CRDs: %v", err)
	}

	for _, crd := range crds.Items {
		// The stored version is marked with storage=true in the status
		for _, version := range crd.Status.StoredVersions {
			key := fmt.Sprintf("%s/%s", crd.Spec.Group, crd.Spec.Names.Kind)
			g.storedVersions[strings.ToLower(key)] = version
		}
	}

	return nil
}

func buildResourceKey(gvr schema.GroupVersionResource, obj metav1.Object) string {
	if obj.GetNamespace() != "" {
		return strings.ToLower(fmt.Sprintf("%s/%s/%s/%s/%s",
			gvr.Group, gvr.Version, gvr.Resource,
			obj.GetNamespace(), obj.GetName()))
	}
	return strings.ToLower(fmt.Sprintf("%s/%s/%s/cluster/%s",
		gvr.Group, gvr.Version, gvr.Resource,
		obj.GetName()))
}
