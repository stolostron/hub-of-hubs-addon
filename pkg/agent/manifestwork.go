package agent

import (
	"os"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"

	workv1 "open-cluster-management.io/api/work/v1"
)

const (
	HOH_AGENT = "hoh-agent"
)

func CreateHohAgentManifestwork(namespace, boostrapServer, SSLCA string) (*workv1.ManifestWork, error) {

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
		},
		Spec: workv1.ManifestWorkSpec{
			Workload: workv1.ManifestsTemplate{
				Manifests: manifests,
			},
		},
	}, nil
}
