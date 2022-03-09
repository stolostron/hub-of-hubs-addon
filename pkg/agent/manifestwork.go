package agent

import (
	"encoding/json"
	"os"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	"sigs.k8s.io/yaml"

	workv1 "open-cluster-management.io/api/work/v1"
)

const (
	HOH_AGENT = "hoh-agent"
)

func CreateHohAgentManifestwork(namespace, boostrapServer, SSLCA string) (*workv1.ManifestWork, error) {

	hoh_version := "latest"
	if os.Getenv("HUB_OF_HUBS_VERSION") != "" {
		hoh_version = os.Getenv("HUB_OF_HUBS_VERSION")
	}

	entries, err := os.ReadDir("manifests")
	if err != nil {
		return nil, err
	}
	var files [][]byte
	for _, entry := range entries {
		file, err := os.ReadFile("manifests/" + entry.Name())
		if err != nil {
			return nil, err
		}
		fileStr := strings.ReplaceAll(string(file), "$LH_ID", namespace)
		fileStr = strings.ReplaceAll(fileStr, "$KAFKA_BOOTSTRAP_SERVERS", boostrapServer)
		fileStr = strings.ReplaceAll(fileStr, "$KAFKA_SSL_CA", SSLCA)
		fileStr = strings.ReplaceAll(fileStr, "-sync:latest", "-sync:"+hoh_version)
		fileJson, err := yaml.YAMLToJSON([]byte(fileStr))
		if err != nil {
			return nil, err
		}
		files = append(files, fileJson)
	}

	var manifests []workv1.Manifest
	for _, file := range files {
		manifests = append(manifests, workv1.Manifest{RawExtension: runtime.RawExtension{
			Raw: file,
		}})
	}

	return &workv1.ManifestWork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      namespace + "-" + HOH_AGENT,
			Namespace: namespace,
			Labels: map[string]string{
				"hub-of-hubs.open-cluster-management.io/managed-by": "hoh-addon",
			},
		},
		Spec: workv1.ManifestWorkSpec{
			Workload: workv1.ManifestsTemplate{
				Manifests: manifests,
			},
		},
	}, nil
}

func EnsureManifestWork(existing, desired *workv1.ManifestWork) (bool, error) {
	// compare the manifests
	existingBytes, err := json.Marshal(existing.Spec)
	if err != nil {
		return false, err
	}
	desiredBytes, err := json.Marshal(desired.Spec)
	if err != nil {
		return false, err
	}
	if string(existingBytes) != string(desiredBytes) {
		klog.V(2).Infof("the existing manifestwork is %s", string(existingBytes))
		klog.V(2).Infof("the desired manifestwork is %s", string(desiredBytes))
		return true, nil
	}
	return false, nil
}
