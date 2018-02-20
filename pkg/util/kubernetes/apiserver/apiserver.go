// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2018 Datadog, Inc.

// +build kubeapiserver

package apiserver

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"io/ioutil"

	log "github.com/cihub/seelog"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/DataDog/datadog-agent/pkg/config"
	"github.com/DataDog/datadog-agent/pkg/util/cache"
	"github.com/DataDog/datadog-agent/pkg/util/retry"
)

var (
	globalAPIClient      *APIClient
	globalTimeoutSeconds = int64(5)
	ErrNotFound          = errors.New("entity not found")
	ErrOutdated          = errors.New("entity is outdated")
	ErrNotLeader         = errors.New("not Leader")
)

const (
	configMapDCAToken         = "datadogtoken"
	tokenTime                 = "tokenTimestamp"
	tokenKey                  = "tokenKey"
	metadataPollIntl          = 20 * time.Second
	metadataMapExpire         = 5 * time.Minute
	metadataMapperCachePrefix = "KubernetesMetadataMapping"
)

// APIClient provides authenticated access to the
// apiserver endpoints. Use the shared instance via GetApiClient.
type APIClient struct {
	// used to setup the APIClient
	initRetry retry.Retrier

	client  *corev1.CoreV1Client
	timeout time.Duration
}

// GetAPIClient returns the shared ApiClient instance.
func GetAPIClient() (*APIClient, error) {
	if globalAPIClient == nil {
		globalAPIClient = &APIClient{
			// TODO: make it configurable if requested
			timeout: 5 * time.Second,
		}
		globalAPIClient.initRetry.SetupRetrier(&retry.Config{
			Name:          "apiserver",
			AttemptMethod: globalAPIClient.connect,
			Strategy:      retry.RetryCount,
			RetryCount:    10,
			RetryDelay:    30 * time.Second,
		})
	}
	err := globalAPIClient.initRetry.TriggerRetry()
	if err != nil {
		log.Debugf("init error: %s", err)
		return nil, err
	}
	return globalAPIClient, nil
}

// GetClient returns an official Kubernetes core v1 client
func GetClient() (*corev1.CoreV1Client, error) {
	var k8sConfig *rest.Config
	var err error

	cfgPath := config.Datadog.GetString("kubernetes_kubeconfig_path")
	if cfgPath == "" {
		k8sConfig, err = rest.InClusterConfig()
		if err != nil {
			log.Debug("Can't create a config for the official client from the service account's token: %s", err)
			return nil, err
		}
	} else {
		// use the current context in kubeconfig
		k8sConfig, err = clientcmd.BuildConfigFromFlags("", cfgPath)
		if err != nil {
			log.Debug("Can't create a config for the official client from the configured path to the kubeconfig: %s, ", cfgPath, err)
			return nil, err
		}
	}

	k8sConfig.Timeout = 2 * time.Second

	coreClient, err := corev1.NewForConfig(k8sConfig)

	return coreClient, err
}
func (c *APIClient) connect() error {
	var err error
	if c.client == nil {
		c.client, err = GetClient()
		if err != nil {
			log.Errorf("Not Able to set up a client for the Leader Election: %s", err)
			return err
		}
	}

	// Try to get apiserver version to confim connectivity
	APIversion := c.client.RESTClient().APIVersion()

	if !APIversion.Empty() {
		log.Debugf("Cannot retrieve the version of the API server at the moment, retrying...")
		return nil
	}

	log.Debugf("Connected to kubernetes apiserver, version %s", APIversion.Version)

	err = c.checkResourcesAuth()
	if err != nil {
		return err
	}
	log.Debug("Could successfully collect Pods, Nodes, Services and Events.")

	useMetadataMapper := config.Datadog.GetBool("use_metadata_mapper")
	if !useMetadataMapper {
		return nil
	}

	return nil
}

// MetadataMapperBundle maps the podNames to the metadata they are associated with.
// ex: services.
// It is updated by mapServices in services.go
// example: [ "pod" : ["svc1","svc2"]]
type MetadataMapperBundle struct {
	PodNameToService map[string][]string `json:"services,omitempty"`
	m                sync.RWMutex
}

func newMetadataMapperBundle() *MetadataMapperBundle {
	return &MetadataMapperBundle{
		PodNameToService: make(map[string][]string),
	}
}

// NodeMetadataMapping only fetch the endpoints from Kubernetes apiserver and add the metadataMapper of the
// node to the cache
// Only called when the node agent computes the metadata mapper locally and does not rely on the DCA.
func (c *APIClient) NodeMetadataMapping(nodeName string, podList *v1.PodList) error {
	endpointList, err := c.client.Endpoints("").List(metav1.ListOptions{TimeoutSeconds: &globalTimeoutSeconds})
	if err != nil {
		log.Errorf("Could not collect endpoints from the API Server: %q", err.Error())
		return err
	}
	if endpointList.Items == nil {
		log.Debug("No endpoints collected from the API server")
		return nil
	}
	log.Debugf("Successfully collected endpoints")

	var node v1.Node
	var nodeList v1.NodeList
	node.Name = nodeName

	nodeList.Items = append(nodeList.Items, node)

	processKubeServices(&nodeList, podList, endpointList)
	return nil
}

// ClusterMetadataMapping queries the Kubernetes apiserver to get the following resources:
// - all nodes
// - all endpoints of all namespaces
// - all pods of all namespaces
// Then it stores in cache the MetadataMapperBundle of each node.
func (c *APIClient) ClusterMetadataMapping() error {
	// The timeout for the context is the same as the poll frequency.
	// We use a new context at each run, to recover if we can't access the API server temporarily.
	// A poll run should take less than the poll frequency.

	// We fetch nodes to reliably use nodename as key in the cache.
	// Avoiding to retrieve them from the endpoints/podList.
	nodeList, err := c.client.Nodes().List(metav1.ListOptions{TimeoutSeconds: &globalTimeoutSeconds})
	if err != nil {
		log.Errorf("Could not collect nodes from the kube-apiserver: %q", err.Error())
		return err
	}
	if nodeList.Items == nil {
		log.Debug("No node collected from the kube-apiserver")
		return nil
	}

	endpointList, err := c.client.Endpoints("").List(metav1.ListOptions{TimeoutSeconds: &globalTimeoutSeconds})
	if err != nil {
		log.Errorf("Could not collect endpoints from the kube-apiserver: %q", err.Error())
		return err
	}
	if endpointList.Items == nil {
		log.Debug("No endpoint collected from the kube-apiserver")
		return nil
	}

	podList, err := c.client.Pods("").List(metav1.ListOptions{TimeoutSeconds: &globalTimeoutSeconds})
	if err != nil {
		log.Errorf("Could not collect pods from the kube-apiserver: %q", err.Error())
		return err
	}
	if podList.Items == nil {
		log.Debug("No pod collected from the kube-apiserver")
		return nil
	}

	processKubeServices(nodeList, podList, endpointList)
	return nil
}

// processKubeServices adds services to the metadataMapper cache, pointer parameters must be non nil
func processKubeServices(nodeList *v1.NodeList, podList *v1.PodList, endpointList *v1.EndpointsList) {
	if nodeList.Items == nil || podList.Items == nil || endpointList.Items == nil {
		return
	}
	log.Debugf("Identified: %d node, %d pod, %d endpoints", len(nodeList.Items), len(podList.Items), len(endpointList.Items))
	for _, node := range nodeList.Items {
		nodeName := node.Name
		nodeNameCacheKey := cache.BuildAgentKey(metadataMapperCachePrefix, nodeName)
		metaBundle, found := cache.Cache.Get(nodeNameCacheKey)
		if !found {
			metaBundle = newMetadataMapperBundle()
		}
		err := metaBundle.(*MetadataMapperBundle).mapServices(nodeName, *podList, *endpointList)
		if err != nil {
			log.Errorf("Could not map the services: %s on node %s", err.Error(), node.Name)
			continue
		}
		cache.Cache.Set(nodeNameCacheKey, metaBundle, metadataMapExpire)
	}
}

// StartMetadataMapping is only called once, when we have confirmed we could correctly connect to the API server.
// The logic here is solely to retrieve Nodes, Pods and Endpoints. The processing part is in mapServices.
func (c *APIClient) StartMetadataMapping() {
	tickerSvcProcess := time.NewTicker(metadataPollIntl)
	go func() {
		for {
			select {
			case <-tickerSvcProcess.C:
				c.ClusterMetadataMapping()
			}
		}
	}()
}

func aggregateCheckResourcesErrors(errorMessages []string) error {
	if len(errorMessages) == 0 {
		return nil
	}
	return fmt.Errorf("check resources failed: %s", strings.Join(errorMessages, ", "))
}

// checkResourcesAuth is meant to check that we can query resources from the API server.
// Depending on the user's config we only trigger an error if necessary.
// The Event check requires getting Events data.
// The MetadataMapper case, requires access to Services, Nodes and Pods.
func (c *APIClient) checkResourcesAuth() error {
	var errorMessages []string

	resourceTimeoutSeconds := int64(2)

	// We always want to collect events
	_, err := c.client.Events("").List(metav1.ListOptions{Limit: 1, TimeoutSeconds: &resourceTimeoutSeconds})
	if err != nil {
		errorMessages = append(errorMessages, fmt.Sprintf("event collection: %q", err.Error()))
	}

	if config.Datadog.GetBool("use_metadata_mapper") == false {
		return aggregateCheckResourcesErrors(errorMessages)
	}
	_, err = c.client.Services("").List(metav1.ListOptions{Limit: 1, TimeoutSeconds: &resourceTimeoutSeconds})
	if err != nil {
		errorMessages = append(errorMessages, fmt.Sprintf("service collection: %q", err.Error()))
	}
	_, err = c.client.Pods("").List(metav1.ListOptions{Limit: 1, TimeoutSeconds: &resourceTimeoutSeconds})
	if err != nil {
		errorMessages = append(errorMessages, fmt.Sprintf("pod collection: %q", err.Error()))
	}
	_, err = c.client.Nodes().List(metav1.ListOptions{Limit: 1, TimeoutSeconds: &resourceTimeoutSeconds})
	if err != nil {
		errorMessages = append(errorMessages, fmt.Sprintf("node collection: %q", err.Error()))
	}
	return aggregateCheckResourcesErrors(errorMessages)
}

// ComponentStatuses returns the component status list from the APIServer
func (c *APIClient) ComponentStatuses() (*v1.ComponentStatusList, error) {
	return c.client.ComponentStatuses().List(metav1.ListOptions{TimeoutSeconds: &globalTimeoutSeconds})
}

// GetTokenFromConfigmap returns the value of the `tokenValue` from the `tokenKey` in the ConfigMap `configMapDCAToken` if its timestamp is less than tokenTimeout old.
func (c *APIClient) GetTokenFromConfigmap(token string, tokenTimeout int64) (string, bool, error) {
	namespace := GetResourcesNamespace()
	tokenConfigMap, err := c.client.ConfigMaps(namespace).Get(configMapDCAToken, metav1.GetOptions{})
	if err != nil {
		log.Debugf("Could not find the ConfigMap %s: %s", configMapDCAToken, err.Error())
		return "", false, ErrNotFound
	}
	log.Infof("Found the ConfigMap %s", configMapDCAToken)

	eventTokenKey := fmt.Sprintf("%s.%s", token, tokenKey)
	tokenValue, found := tokenConfigMap.Data[eventTokenKey]
	if !found {
		log.Errorf("%s was not found in the ConfigMap %s", eventTokenKey, configMapDCAToken)
		return "", found, ErrNotFound
	}
	log.Infof("%s is %q", token, tokenValue)

	eventTokenTS := fmt.Sprintf("%s.%s", token, tokenTime)
	tokenTimeStr, set := tokenConfigMap.Data[eventTokenTS] // This is so we can have one timestamp per token

	if !set {
		log.Debugf("Could not find timestamp associated with %s in the ConfigMap %s. Refreshing.", eventTokenTS, configMapDCAToken)
		// We return ErrOutdated to reset the tokenValue and its timestamp as token's timestamp was not found.
		return tokenValue, found, ErrOutdated
	}

	tokenTime, err := time.Parse(time.RFC822, tokenTimeStr)
	if err != nil {
		return "", found, log.Errorf("could not convert the timestamp associated with %s from the ConfigMap %s", token, configMapDCAToken)
	}
	tokenAge := time.Now().Unix() - tokenTime.Unix()

	if tokenAge > tokenTimeout {
		log.Debugf("The tokenValue %s is outdated, refreshing the state", token)
		return tokenValue, found, ErrOutdated
	}
	log.Debugf("Token %s was updated recently, using value to collect newer events.", token)
	return tokenValue, found, nil
}

// UpdateTokenInConfigmap updates the value of the `tokenValue` from the `tokenKey` and
// sets its collected timestamp in the ConfigMap `configmaptokendca`
func (c *APIClient) UpdateTokenInConfigmap(token, tokenValue string) error {
	namespace := GetResourcesNamespace()
	tokenConfigMap, err := c.client.ConfigMaps(namespace).Get(configMapDCAToken, metav1.GetOptions{})
	if err != nil {
		return err
	}

	eventTokenKey := fmt.Sprintf("%s.%s", token, tokenKey)
	tokenConfigMap.Data[eventTokenKey] = tokenValue

	now := time.Now()
	eventTokenTS := fmt.Sprintf("%s.%s", token, tokenTime)
	tokenConfigMap.Data[eventTokenTS] = now.Format(time.RFC822) // Timestamps in the ConfigMap should all use the type int.

	_, err = c.client.ConfigMaps(namespace).Update(tokenConfigMap)
	if err != nil {
		return err
	}
	log.Debugf("Updated %s to %s in the ConfigMap %s", eventTokenKey, tokenValue, configMapDCAToken)
	return nil
}

// NodeLabels is used to fetch the labels attached to a given node.
func (c *APIClient) NodeLabels(nodeName string) (map[string]string, error) {
	node, err := c.client.Nodes().Get(nodeName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return node.Labels, nil // GetMetadata().GetLabels(), nil
}

// GetMetadataMapBundleOnAllNodes is used for the CLI svcmap command to run fetch the metadata map of all nodes.
func GetMetadataMapBundleOnAllNodes() (map[string]interface{}, error) {
	nodePodMetadataMap := make(map[string]*MetadataMapperBundle)
	stats := make(map[string]interface{})
	var warnlist []string
	var warn string
	var err error

	nodes, err := getNodeList()
	if err != nil {
		stats["Errors"] = fmt.Sprintf("Failed to get nodes from the API server: %s", err.Error())
		return stats, err
	}

	for _, node := range nodes {
		if node.GetObjectMeta() == nil {
			log.Error("Incorrect payload when evaluating a node for the service mapper") // This will be removed as we move to the client-go
			continue
		}
		nodePodMetadataMap[node.Name], err = getMetadataMapBundle(node.Name)
		if err != nil {
			warn = fmt.Sprintf("Node %s could not be added to the service map bundle: %s", node.Name, err.Error())
			warnlist = append(warnlist, warn)
		}
	}
	stats["Nodes"] = nodePodMetadataMap
	stats["Warnings"] = warnlist
	return stats, nil
}

// GetMetadataMapBundleOnNode is used for the CLI metamap command to output given a nodeName.
func GetMetadataMapBundleOnNode(nodeName string) (map[string]interface{}, error) {
	nodePodMetadataMap := make(map[string]*MetadataMapperBundle)
	stats := make(map[string]interface{})
	var err error

	nodePodMetadataMap[nodeName], err = getMetadataMapBundle(nodeName)
	if err != nil {
		stats["Warnings"] = []string{fmt.Sprintf("Node %s could not be added to the metadata map bundle: %s", nodeName, err.Error())}
		return stats, err
	}
	stats["Nodes"] = nodePodMetadataMap
	return stats, nil
}

func getMetadataMapBundle(nodeName string) (*MetadataMapperBundle, error) {
	nodeNameCacheKey := cache.BuildAgentKey(metadataMapperCachePrefix, nodeName)
	metaBundle, found := cache.Cache.Get(nodeNameCacheKey)
	if !found {
		return nil, fmt.Errorf("the key %s was not found in the cache", nodeNameCacheKey)
	}
	return metaBundle.(*MetadataMapperBundle), nil
}

func getNodeList() ([]v1.Node, error) {
	cl, err := GetAPIClient()
	if err != nil {
		log.Errorf("Can't create client to query the API Server: %s", err.Error())
		return nil, err
	}
	nodes, err := cl.client.Nodes().List(metav1.ListOptions{TimeoutSeconds: &globalTimeoutSeconds})

	if err != nil {
		log.Errorf("Can't list nodes from the API server: %s", err.Error())
		return nil, err
	}
	return nodes.Items, nil
}

// GetResourcesNamespace is used to fetch the namespace of the resources used by the Kubernetes check (e.g. Leader Election, Event collection).
func GetResourcesNamespace() string {
	namespace := config.Datadog.GetString("kube_resources_namespace")
	if namespace != "" {
		return namespace
	}
	log.Debugf("No configured namespace for the resource, fetching from the current context")
	namespacePath := "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
	val, e := ioutil.ReadFile(namespacePath)
	if e == nil && val != nil {
		return string(val)
	}
	log.Errorf("There was an error fetching the namespace from the context, using default")
	return "default"
}
