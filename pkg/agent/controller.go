package agent

import (
	"context"
	"encoding/base64"

	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"k8s.io/apimachinery/pkg/api/errors"
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

const (
	HOH_HUB_CLUSTER_MCH = "hoh-hub-cluster-mch"
)

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
				// only enqueue when the hoh=enabled managed cluster is changed
				if accessor.GetName() == accessor.GetNamespace()+"-"+HOH_AGENT {
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
	_, err := c.clusterLister.Get(managedClusterName)
	if errors.IsNotFound(err) {
		// Spoke cluster not found, could have been deleted, delete manifestwork.
		// TODO: delete manifestwork
		return nil
	}
	if err != nil {
		return err
	}

	// if mch is running, then install hoh agent
	mch, err := c.workLister.ManifestWorks(managedClusterName).Get(managedClusterName + "-" + HOH_HUB_CLUSTER_MCH)
	if err != nil {
		return err
	}

	// if the MCH is Running, then create hoh agent manifestwork to install HoH agent
	for _, conditions := range mch.Status.ResourceStatus.Manifests {
		if conditions.ResourceMeta.Kind == "MultiClusterHub" {
			for _, value := range conditions.StatusFeedbacks.Values {
				if value.Name == "state" && *value.Value.String == "Running" {
					bootstrapServers, cert, err := c.getKafkaSSLCA()
					if err != nil {
						return err
					}

					desiredAgent, err := CreateHohAgentManifestwork(managedClusterName, bootstrapServers, cert)
					if err != nil {
						return err
					}

					agent, err := c.workLister.ManifestWorks(managedClusterName).Get(managedClusterName + "-" + HOH_AGENT)
					if errors.IsNotFound(err) {
						klog.V(2).Infof("creating hoh agent manifestwork in %s namespace", managedClusterName)
						_, err := c.workClient.ManifestWorks(managedClusterName).
							Create(ctx, desiredAgent, metav1.CreateOptions{})
						if err != nil {
							klog.V(2).ErrorS(err, "failed to create hoh agent manifestwork", "manifestwork is", desiredAgent)
							return err
						}
						return nil
					}
					if err != nil {
						return err
					}

					updated, err := EnsureManifestWork(agent, desiredAgent)
					if err != nil {
						return err
					}
					if updated {
						desiredAgent.ObjectMeta.ResourceVersion = agent.ObjectMeta.ResourceVersion
						_, err := c.workClient.ManifestWorks(managedClusterName).
							Update(ctx, desiredAgent, metav1.UpdateOptions{})
						if err != nil {
							return err
						}
					}
				}
			}
		}
	}

	return nil
}

func (c *hohAgentController) getKafkaSSLCA() (string, string, error) {
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
