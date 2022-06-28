package agent

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"io"
	"io/fs"
	"strings"
	"text/template"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/klog/v2"

	workclientv1 "open-cluster-management.io/api/client/work/clientset/versioned/typed/work/v1"
	worklisterv1 "open-cluster-management.io/api/client/work/listers/work/v1"
	workv1 "open-cluster-management.io/api/work/v1"
)

func CreateHohAgentManifestwork(tpl *template.Template, agentConfigValues *agentConfig) (*workv1.ManifestWork, error) {
	var buf bytes.Buffer
	tpl.ExecuteTemplate(&buf, "manifests/nonhypershift", *agentConfigValues)

	var manifests []workv1.Manifest
	yamlReader := yaml.NewYAMLReader(bufio.NewReader(&buf))
	for {
		b, err := yamlReader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(b) != 0 {
			rawJSON, err := yaml.ToJSON(b)
			if err != nil {
				return nil, err
			}
			if string(rawJSON) != "null" {
				klog.V(2).Infof("raw JSON for nonhypershift:\n%s\n", rawJSON)
				manifests = append(manifests, workv1.Manifest{RawExtension: runtime.RawExtension{Raw: rawJSON}})
			}
		}
	}

	return &workv1.ManifestWork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      agentConfigValues.LeadHubID + "-" + hohAgent,
			Namespace: agentConfigValues.LeadHubID,
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

func CreateHohAgentManifestworkOnHyperMgt(tpl *template.Template, agentConfigValues *agentConfig, hostingClusterName string) (*workv1.ManifestWork, error) {
	var buf bytes.Buffer
	tpl.ExecuteTemplate(&buf, "manifests/hypershift/management", *agentConfigValues)

	var manifests []workv1.Manifest
	yamlReader := yaml.NewYAMLReader(bufio.NewReader(&buf))
	for {
		b, err := yamlReader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(b) != 0 {
			rawJSON, err := yaml.ToJSON(b)
			if err != nil {
				return nil, err
			}
			if string(rawJSON) != "null" {
				klog.V(2).Infof("raw JSON for hypershift management cluster:\n%s\n", rawJSON)
				manifests = append(manifests, workv1.Manifest{RawExtension: runtime.RawExtension{Raw: rawJSON}})
			}
		}
	}

	return &workv1.ManifestWork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      agentConfigValues.LeadHubID + "-" + hohAgentMgt,
			Namespace: hostingClusterName,
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

func CreateHohAgentManifestworkOnHyperHosted(tpl *template.Template, agentConfigValues *agentConfig, hostingClusterName string) (*workv1.ManifestWork, error) {
	var buf bytes.Buffer
	tpl.ExecuteTemplate(&buf, "manifests/hypershift/hosted", *agentConfigValues)

	var manifests []workv1.Manifest
	yamlReader := yaml.NewYAMLReader(bufio.NewReader(&buf))
	for {
		b, err := yamlReader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(b) != 0 {
			rawJSON, err := yaml.ToJSON(b)
			if err != nil {
				return nil, err
			}
			if string(rawJSON) != "null" {
				klog.V(2).Infof("raw JSON for hypershift hosted cluster:\n%s\n", rawJSON)
				manifests = append(manifests, workv1.Manifest{RawExtension: runtime.RawExtension{Raw: rawJSON}})
			}
		}
	}

	return &workv1.ManifestWork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      agentConfigValues.LeadHubID + "-" + hohAgentHosted,
			Namespace: agentConfigValues.LeadHubID,
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
	if !equality.Semantic.DeepDerivative(desired.Spec.DeleteOption, existing.Spec.DeleteOption) {
		return true, nil
	}

	if !equality.Semantic.DeepDerivative(desired.Spec.ManifestConfigs, existing.Spec.ManifestConfigs) {
		return true, nil
	}

	if len(existing.Spec.Workload.Manifests) != len(desired.Spec.Workload.Manifests) {
		return true, nil
	}

	for i, m := range existing.Spec.Workload.Manifests {
		var existingObj, desiredObj interface{}
		if len(m.RawExtension.Raw) > 0 {
			if err := json.Unmarshal(m.RawExtension.Raw, &existingObj); err != nil {
				return false, err
			}
		} else {
			existingObj = m.RawExtension.Object
		}

		if len(desired.Spec.Workload.Manifests[i].RawExtension.Raw) > 0 {
			if err := json.Unmarshal(desired.Spec.Workload.Manifests[i].RawExtension.Raw, &desiredObj); err != nil {
				return false, err
			}
		} else {
			desiredObjBytes, err := json.Marshal(desired.Spec.Workload.Manifests[i].RawExtension.Object)
			if err != nil {
				return false, err
			}
			if err := json.Unmarshal(desiredObjBytes, &desiredObj); err != nil {
				return false, err
			}
		}

		metadata := existingObj.(map[string]interface{})["metadata"].(map[string]interface{})
		metadata["creationTimestamp"] = nil
		if !equality.Semantic.DeepDerivative(desiredObj, existingObj) {
			klog.V(2).Infof("existing manifest object %d is not equal to the desired manifest object", i)
			return true, nil
		}
	}

	return false, nil
}

// applyManifestWork creates or updates a single manifestwork resource
func applyManifestWork(ctx context.Context, workclient workclientv1.WorkV1Interface, workLister worklisterv1.ManifestWorkLister,
	manifestWork *workv1.ManifestWork) error {
	existingManifestWork, err := workLister.ManifestWorks(manifestWork.GetNamespace()).Get(manifestWork.GetName())
	if errors.IsNotFound(err) {
		klog.V(2).Infof("creating manifestwork %s in namespace %s", manifestWork.GetName(), manifestWork.GetNamespace())
		_, err := workclient.ManifestWorks(manifestWork.GetNamespace()).
			Create(ctx, manifestWork, metav1.CreateOptions{})
		if err != nil {
			return err
		}
		return nil
	}
	if err != nil {
		return err
	}

	updated, err := EnsureManifestWork(existingManifestWork, manifestWork)
	if err != nil {
		return err
	}

	if updated {
		manifestWork.ObjectMeta.ResourceVersion = existingManifestWork.ObjectMeta.ResourceVersion
		_, err := workclient.ManifestWorks(manifestWork.GetNamespace()).
			Update(ctx, manifestWork, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
	}

	return nil
}

func parseTemplates(manifestFS embed.FS, filter string) (*template.Template, error) {
	tpl := template.New("")
	err := fs.WalkDir(manifestFS, ".", func(file string, d fs.DirEntry, err1 error) error {
		if d.IsDir() && (strings.HasSuffix(file, "nonhypershift") || strings.HasSuffix(file, "hosted") || strings.HasSuffix(file, "management")) {
			if err1 != nil {
				return err1
			}

			manifests, err2 := readFileInDir(manifestFS, file, filter)
			if err2 != nil {
				return err2
			}

			t := tpl.New(file)
			_, err3 := t.Parse(manifests)
			if err3 != nil {
				return err3
			}
		}

		return nil
	})

	return tpl, err
}

func readFileInDir(manifestFS embed.FS, dir, filter string) (string, error) {
	var res string
	err := fs.WalkDir(manifestFS, dir, func(file string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !d.IsDir() {
			if filter == "" || !strings.Contains(file, filter) {
				b, err := manifestFS.ReadFile(file)
				if err != nil {
					return err
				}
				res += string(b) + "\n---\n"
			}
		}

		return nil
	})

	return res, err
}
