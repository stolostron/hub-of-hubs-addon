package pkg

import (
	"context"
	"time"

	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/spf13/cobra"
	"github.com/stolostron/hub-of-hubs-addon-controller/pkg/version"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	clusterv1client "open-cluster-management.io/api/client/cluster/clientset/versioned"
	clusterv1informers "open-cluster-management.io/api/client/cluster/informers/externalversions"
	workv1client "open-cluster-management.io/api/client/work/clientset/versioned"
	workv1informers "open-cluster-management.io/api/client/work/informers/externalversions"

	"github.com/stolostron/hub-of-hubs-addon-controller/pkg/agent"
)

var ResyncInterval = 5 * time.Minute

func NewController() *cobra.Command {
	cmd := controllercmd.
		NewControllerCommandConfig("hub-of-hubs-addon-controller", version.Get(), runControllerManager).
		NewCommand()
	cmd.Use = "controller"
	cmd.Short = "Start the Hub of Hubs Addon Controller"

	return cmd
}

// runControllerManager starts the controllers on hub to manage spoke cluster registration.
func runControllerManager(ctx context.Context, controllerContext *controllercmd.ControllerContext) error {
	// If qps in kubconfig is not set, increase the qps and burst to enhance the ability of kube client to handle
	// requests in concurrent
	// TODO: Use ClientConnectionOverrides flags to change qps/burst when library-go exposes them in the future
	kubeConfig := rest.CopyConfig(controllerContext.KubeConfig)
	if kubeConfig.QPS == 0.0 {
		kubeConfig.QPS = 100.0
		kubeConfig.Burst = 200
	}

	dynamicClient, err := dynamic.NewForConfig(kubeConfig)
	if err != nil {
		return err
	}

	clusterClient, err := clusterv1client.NewForConfig(kubeConfig)
	if err != nil {
		return err
	}

	workClient, err := workv1client.NewForConfig(kubeConfig)
	if err != nil {
		return err
	}

	clusterInformers := clusterv1informers.NewSharedInformerFactory(clusterClient, 10*time.Minute)
	workInformers := workv1informers.NewSharedInformerFactory(workClient, 10*time.Minute)

	agentController := agent.NewHohAgentController(
		dynamicClient,
		workClient.WorkV1(),
		clusterInformers.Cluster().V1().ManagedClusters(),
		workInformers.Work().V1().ManifestWorks(),
		controllerContext.EventRecorder,
	)

	go clusterInformers.Start(ctx.Done())
	go workInformers.Start(ctx.Done())

	go agentController.Run(ctx, 1)

	<-ctx.Done()
	return nil
}
