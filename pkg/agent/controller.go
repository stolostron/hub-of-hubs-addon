package agent

import (
	"context"
	"encoding/base64"
	"os"

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
	hohHubClusterMCH = "hoh-hub-cluster-mch"
	hohAgent         = "hoh-agent"
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
	if !managedCluster.DeletionTimestamp.IsZero() {
		// the managed cluster is deleting, we should not re-apply the manifestwork
		// wait for managedcluster-import-controller to clean up the manifestwork
		return nil
	}
	// mch only installs in OpenShift cluster
	if managedCluster.GetLabels()["vendor"] == "OpenShift" {
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

	transport_type := "kafka"
	if os.Getenv("TRANSPORT_TYPE") != "" {
		transport_type = os.Getenv("TRANSPORT_TYPE")
	}
	var serverHost, cert string
	if transport_type == "kafka" {
		serverHost, cert, err = c.getKafkaSSLCA()
		if err != nil {
			return err
		}
	} else {
		serverHost, err = c.getCSSHost()
		if err != nil {
			return err
		}
	}

	desiredAgent, err := CreateHohAgentManifestwork(managedClusterName, transport_type, serverHost, cert)
	if err != nil {
		return err
	}

	agent, err := c.workLister.ManifestWorks(managedClusterName).Get(managedClusterName + "-" + hohAgent)
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
