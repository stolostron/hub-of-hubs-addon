package agent

import (
	"context"
	"embed"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"text/template"

	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/klog/v2"

	clusterinformerv1 "open-cluster-management.io/api/client/cluster/informers/externalversions/cluster/v1"
	clusterlisterv1 "open-cluster-management.io/api/client/cluster/listers/cluster/v1"
	workclientv1 "open-cluster-management.io/api/client/work/clientset/versioned/typed/work/v1"
	workinformerv1 "open-cluster-management.io/api/client/work/informers/externalversions/work/v1"
	worklisterv1 "open-cluster-management.io/api/client/work/listers/work/v1"
)

//go:embed manifests
var manifestFS embed.FS

const (
	hohHubClusterMCH = "hoh-hub-cluster-mch"
	hohAgent         = "hoh-agent"
	hohAgentHosted   = "hoh-agent-hosted"
	hohAgentMgt      = "hoh-agent-management"
)

type agentConfig struct {
	AgentVersion         string
	LeadHubID            string
	EnableHoHRBAC        string
	TransportType        string
	KafkaBootstrapServer string
	KafkaCA              string
	CSSHost              string
	HostedClusterName    string // for hypershift
}

// hohAgentController reconciles instances of ManagedCluster on the hub.
type hohAgentController struct {
	dynamicClient dynamic.Interface
	workClient    workclientv1.WorkV1Interface
	clusterLister clusterlisterv1.ManagedClusterLister
	workLister    worklisterv1.ManifestWorkLister
	cache         resourceapply.ResourceCache
	eventRecorder events.Recorder
}

// NewHohAgentController creates a new hub cluster controller
func NewHohAgentController(
	dynamicClient dynamic.Interface,
	workClient workclientv1.WorkV1Interface,
	clusterInformer clusterinformerv1.ManagedClusterInformer,
	workInformer workinformerv1.ManifestWorkInformer,
	recorder events.Recorder) factory.Controller {
	c := &hohAgentController{
		dynamicClient: dynamicClient,
		workClient:    workClient,
		clusterLister: clusterInformer.Lister(),
		workLister:    workInformer.Lister(),
		cache:         resourceapply.NewResourceCache(),
		eventRecorder: recorder.WithComponentSuffix("hub-of-hubs-addon-controller"),
	}
	return factory.New().
		WithFilteredEventsInformersQueueKeyFunc(
			func(obj runtime.Object) string {
				accessor, _ := meta.Accessor(obj)
				return accessor.GetName()
			},
			func(obj interface{}) bool {
				accessor, err := meta.Accessor(obj)
				if err != nil {
					return false
				}
				// enqueue all managed cluster except for local-cluster and hoh=disabled
				if accessor.GetLabels()["hoh"] == "disabled" || accessor.GetName() == "local-cluster" {
					return false
				} else {
					return true
				}
			}, clusterInformer.Informer()).
		WithFilteredEventsInformersQueueKeyFunc(
			func(obj runtime.Object) string {
				accessor, _ := meta.Accessor(obj)
				return accessor.GetNamespace()
			},
			func(obj interface{}) bool {
				accessor, err := meta.Accessor(obj)
				if err != nil {
					return false
				}
				// only enqueue if hoh-agent is changed or hoh-hub-cluster-mch is changed
				if accessor.GetName() == accessor.GetNamespace()+"-"+hohAgent ||
					accessor.GetName() == accessor.GetNamespace()+"-"+hohHubClusterMCH {
					return true
				}
				return false
			}, workInformer.Informer()).
		WithSync(c.sync).
		ToController("HubClusterController", recorder)
}

func (c *hohAgentController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	managedClusterName := syncCtx.QueueKey()
	klog.V(2).Infof("Reconciling HoH agent for %s", managedClusterName)
	managedCluster, err := c.clusterLister.Get(managedClusterName)
	if err != nil {
		return err
	}

	hostingClusterName, hostedClusterName := "", ""
	annotations := managedCluster.GetAnnotations()
	if val, ok := annotations["import.open-cluster-management.io/klusterlet-deploy-mode"]; ok && val == "Hosted" {
		hostingClusterName, ok = annotations["import.open-cluster-management.io/hosting-cluster-name"]
		if !ok || hostingClusterName == "" {
			return fmt.Errorf("missing hosting-cluster-name in managed cluster.")
		}
		hypershiftdeploymentName, ok := annotations["cluster.open-cluster-management.io/hypershiftdeployment"]
		if !ok || hypershiftdeploymentName == "" {
			return fmt.Errorf("missing hypershiftdeployment name in managed cluster.")
		}
		splits := strings.Split(hypershiftdeploymentName, "/")
		if len(splits) != 2 || splits[1] == "" {
			return fmt.Errorf("bad hypershiftdeployment name in managed cluster.")
		}
		hostedClusterName = splits[1]
	}

	if !managedCluster.DeletionTimestamp.IsZero() {
		// the managed cluster is deleting, we should not re-apply the manifestwork
		// wait for managedcluster-import-controller to clean up the manifestwork
		if hostingClusterName != "" { // for hypershift hosted leaf hub, remove the corresponding manifestworks from hypershift management cluster
			if err := c.workClient.ManifestWorks(hostingClusterName).Delete(ctx, managedClusterName+"-"+hohAgentMgt, metav1.DeleteOptions{}); err != nil {
				return err
			}
		}
		return nil
	}

	// mch only installs in OpenShift cluster
	if managedCluster.GetLabels()["vendor"] == "OpenShift" && hostingClusterName == "" {
		// if mch is running, then install hoh agent
		mch, err := c.workLister.ManifestWorks(managedClusterName).Get(managedClusterName + "-" + hohHubClusterMCH)
		if err != nil {
			return err
		}

		mchIsReadyNum := 0
		// if the MCH is Running, then create hoh agent manifestwork to install HoH agent
		// ideally, the mch status should be in Running state.
		// but due to this bug - https://github.com/stolostron/backlog/issues/20555
		// the mch status can be in Installing for a long time.
		// so here just check the dependencies status is True, then install HoH agent
		for _, conditions := range mch.Status.ResourceStatus.Manifests {
			if conditions.ResourceMeta.Kind == "MultiClusterHub" {
				for _, value := range conditions.StatusFeedbacks.Values {
					// no application-chart in 2.5
					// if value.Name == "application-chart-sub-status" && *value.Value.String == "True" {
					// 	mchIsReadyNum++
					// 	continue
					// }
					if value.Name == "cluster-manager-cr-status" && *value.Value.String == "True" {
						mchIsReadyNum++
						continue
					}
					// for ACM 2.5.
					if value.Name == "multicluster-engine-status" && *value.Value.String == "True" {
						mchIsReadyNum++
						continue
					}
					if value.Name == "grc-sub-status" && *value.Value.String == "True" {
						mchIsReadyNum++
						continue
					}
				}
			}
		}

		if mchIsReadyNum != 2 {
			return nil
		}
	}

	hohVersion := "latest"
	if os.Getenv("HUB_OF_HUBS_VERSION") != "" {
		hohVersion = os.Getenv("HUB_OF_HUBS_VERSION")
	}
	enforceHoHRBAC := "false"
	if os.Getenv("ENFORCE_HOH_RBAC") != "" {
		enforceHoHRBAC = os.Getenv("ENFORCE_HOH_RBAC")
	}
	transportType := "kafka"
	if os.Getenv("TRANSPORT_TYPE") != "" {
		transportType = os.Getenv("TRANSPORT_TYPE")
	}

	agentConfigValues := &agentConfig{
		AgentVersion:  hohVersion,
		LeadHubID:     managedClusterName,
		EnableHoHRBAC: enforceHoHRBAC,
		TransportType: transportType,
	}

	if transportType == "kafka" {
		serverHost, cert, err := c.getKafkaSSLCA()
		if err != nil {
			return err
		}
		agentConfigValues.KafkaBootstrapServer = serverHost
		agentConfigValues.KafkaCA = cert
	} else {
		serverHost, err := c.getCSSHost()
		if err != nil {
			return err
		}
		agentConfigValues.CSSHost = serverHost
	}

	if hostedClusterName != "" {
		agentConfigValues.HostedClusterName = "clusters-" + hostedClusterName
	}

	var tpl *template.Template
	if transportType == "kafka" {
		tpl, err = parseTemplates(manifestFS, "ess")
		if err != nil {
			return err
		}
	} else {
		tpl, err = parseTemplates(manifestFS, "")
		if err != nil {
			return err
		}
	}

	if hostingClusterName == "" { // for non-hypershift hosted leaf hub
		agentManifestwork, err := CreateHohAgentManifestwork(tpl, agentConfigValues)
		if err != nil {
			return err
		}

		if err := applyManifestWork(ctx, c.workClient, c.workLister, agentManifestwork); err != nil {
			return err
		}
	} else { // for hypershift hosted leaf hub
		agentManifestworkOnHyperMgt, err := CreateHohAgentManifestworkOnHyperMgt(tpl, agentConfigValues, hostingClusterName)
		if err != nil {
			return err
		}

		if err := applyManifestWork(ctx, c.workClient, c.workLister, agentManifestworkOnHyperMgt); err != nil {
			return err
		}

		agentManifestworkOnHyperHosted, err := CreateHohAgentManifestworkOnHyperHosted(tpl, agentConfigValues, hostingClusterName)
		if err != nil {
			return err
		}

		if err := applyManifestWork(ctx, c.workClient, c.workLister, agentManifestworkOnHyperHosted); err != nil {
			return err
		}
	}

	return nil
}

func (c *hohAgentController) getKafkaSSLCA() (string, string, error) {
	kafkaBootstrapServer := os.Getenv("KAFKA_BOOTSTRAP_SERVER")
	if kafkaBootstrapServer != "" {
		klog.V(2).Infof("Kafka bootstrap server is %s, certificate is %s", kafkaBootstrapServer, "")
		return kafkaBootstrapServer, "", nil
	}

	kafkaGVR := schema.GroupVersionResource{
		Group:    "kafka.strimzi.io",
		Version:  "v1beta2",
		Resource: "kafkas",
	}
	//TODO: pass kafka namespace and name via environment variables
	kafka, err := c.dynamicClient.Resource(kafkaGVR).Namespace("kafka").
		Get(context.TODO(), "kafka-brokers-cluster", metav1.GetOptions{})
	if err != nil {
		return "", "", err
	}
	kafkaStatus := kafka.Object["status"].(map[string]interface{})
	kafkaListeners := kafkaStatus["listeners"].([]interface{})
	bootstrapServers := kafkaListeners[1].(map[string]interface{})["bootstrapServers"].(string)
	certificates := kafkaListeners[1].(map[string]interface{})["certificates"].([]interface{})
	certificate := base64.RawStdEncoding.EncodeToString([]byte(certificates[0].(string)))
	klog.V(2).Infof("Kafka bootstrap server is %s, certificate is %s", bootstrapServers, certificate)
	return bootstrapServers, certificate, nil
}

func (c *hohAgentController) getCSSHost() (string, error) {
	routeGVR := schema.GroupVersionResource{
		Group:    "route.openshift.io",
		Version:  "v1",
		Resource: "routes",
	}
	//TODO: pass kafka namespace and name via environment variables
	cssRoute, err := c.dynamicClient.Resource(routeGVR).Namespace("sync-service").
		Get(context.TODO(), "sync-service-css", metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	routeStatus := cssRoute.Object["status"].(map[string]interface{})
	routeIngress := routeStatus["ingress"].([]interface{})
	serverHost := routeIngress[0].(map[string]interface{})["host"].(string)
	klog.V(2).Infof("css server is %s", serverHost)
	return serverHost, nil
}
