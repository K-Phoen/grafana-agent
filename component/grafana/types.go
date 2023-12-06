package grafana

const KindDashboard = "Dashboard"
const FolderUIDAnnotation = "grafana.com/folder-uid"

type Resource struct {
	APIVersion  string            `json:"apiVersion"`
	Kind        string            `json:"kind"`
	Metadata    map[string]string `json:"metadata"`
	Annotations map[string]string `json:"annotations"`
	Spec        any               `json:"spec"`
}

func (resource Resource) GetAnnotation(name string) string {
	if resource.Annotations == nil {
		return ""
	}

	return resource.Annotations[name]
}

type RawResources []byte
