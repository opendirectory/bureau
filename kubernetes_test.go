package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

func writeKubeconfig(t *testing.T, namespace string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "kubeconfig")
	data := `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: https://kube.example.com:6443
users:
- name: u
  user:
    token: kubeconfig-token
contexts:
- name: ctx
  context:
    cluster: c
    user: u
    namespace: ` + namespace + `
current-context: ctx
`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// stubInCluster replaces the in-cluster config loader and namespace file
func stubInCluster(t *testing.T, cfg *rest.Config, err error, namespace string) {
	t.Helper()

	origCfg, origNS, origK8s := inClusterConfig, serviceAccountNamespaceFile, C.Kubernetes
	t.Cleanup(func() {
		inClusterConfig, serviceAccountNamespaceFile, C.Kubernetes = origCfg, origNS, origK8s
	})

	inClusterConfig = func() (*rest.Config, error) { return cfg, err }
	serviceAccountNamespaceFile = filepath.Join(t.TempDir(), "namespace")
	if namespace != "" {
		if err := os.WriteFile(serviceAccountNamespaceFile, []byte(namespace+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestK8sRestConfigDefaultsToServiceAccount(t *testing.T) {
	// a valid kubeconfig in the default locations must be ignored
	t.Setenv("KUBECONFIG", writeKubeconfig(t, "from-kubeconfig"))

	stubInCluster(t, nil, rest.ErrNotInCluster, "")
	C.Kubernetes = KubernetesConfig{Enabled: true}
	if _, _, err := K8sRestConfig(); !errors.Is(err, rest.ErrNotInCluster) {
		t.Fatalf("expected in-cluster error without falling back to $KUBECONFIG, got %v", err)
	}

	stubInCluster(t, &rest.Config{Host: "https://10.0.0.1:443", BearerToken: "sa-token"}, nil, "bureau-ns")
	C.Kubernetes = KubernetesConfig{Enabled: true}
	cfg, ns, err := K8sRestConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BearerToken != "sa-token" || ns != "bureau-ns" {
		t.Errorf("got host=%s token=%s ns=%s, want service account config in bureau-ns", cfg.Host, cfg.BearerToken, ns)
	}

	// explicit namespace wins over the pod's
	C.Kubernetes.Namespace = "other"
	if _, ns, _ = K8sRestConfig(); ns != "other" {
		t.Errorf("namespace = %s, want other", ns)
	}

	// no namespace anywhere
	stubInCluster(t, &rest.Config{Host: "https://10.0.0.1:443"}, nil, "")
	C.Kubernetes = KubernetesConfig{Enabled: true}
	if _, _, err = K8sRestConfig(); err == nil {
		t.Errorf("expected error when pod namespace is unavailable")
	}
}

func TestK8sRestConfigExplicitKubeconfig(t *testing.T) {
	stubInCluster(t, nil, errors.New("in-cluster config must not be used"), "")
	C.Kubernetes = KubernetesConfig{Enabled: true, Kubeconfig: writeKubeconfig(t, "ctx-ns")}

	cfg, ns, err := K8sRestConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "https://kube.example.com:6443" || cfg.BearerToken != "kubeconfig-token" || ns != "ctx-ns" {
		t.Errorf("got host=%s token=%s ns=%s", cfg.Host, cfg.BearerToken, ns)
	}

	C.Kubernetes.Kubeconfig = filepath.Join(t.TempDir(), "missing")
	if _, _, err = K8sRestConfig(); err == nil {
		t.Errorf("expected error for missing kubeconfig")
	}
}

func TestConfigMapName(t *testing.T) {
	C.Base = "ou=config,dc=example,dc=com"
	C.Kubernetes.Prefix = ""

	cases := map[string]string{
		"cn=bureau,cn=web01,ou=config,dc=example,dc=com":       "bureau-web01-bureau",
		"cn=My_File.conf,cn=WEB01,ou=config,dc=example,dc=com": "bureau-web01-my-file.conf",
		"cn=other,dc=elsewhere,dc=com":                         "bureau-com-elsewhere-other",
		"cn=.bashrc,cn=web01,ou=config,dc=example,dc=com":      "bureau-web01.bashrc",
		"cn=a.-b,cn=web01,ou=config,dc=example,dc=com":         "bureau-web01-a.b",
	}

	for dn, want := range cases {
		if got := ConfigMapName(dn); got != want {
			t.Errorf("ConfigMapName(%q) = %q, want %q", dn, got, want)
		}
	}
}

func TestConfigMapKey(t *testing.T) {
	cases := map[string]string{
		"/etc/keepalived/keepalived.conf": "keepalived.conf",
		"/tmp/bureau file.txt":            "bureau_file.txt",
		"":                                k8sDefaultKey,
		"/":                               k8sDefaultKey,
		`C:\ProgramData\app\app.ini`:      "app.ini",
		`%APPDATA%\app\settings.json`:     "settings.json",
		`C:\`:                             k8sDefaultKey,
		"~/.bashrc":                       ".bashrc",
	}

	for path, want := range cases {
		if got := ConfigMapKey(path); got != want {
			t.Errorf("ConfigMapKey(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestApplyConfigMap(t *testing.T) {
	C.Base = "ou=config,dc=example,dc=com"
	client := fake.NewSimpleClientset()
	ctx := context.Background()

	entry := ConfigMapEntry{
		DN:    "cn=bureau,cn=web01,ou=config,dc=example,dc=com",
		Path:  "/tmp/bureau.txt",
		Data:  "Hello world!",
		Perm:  "0644",
		Mtime: "20240101000000Z",
	}

	// create
	if err := ApplyConfigMap(client, "default", entry); err != nil {
		t.Fatalf("create: %v", err)
	}
	cm, err := client.CoreV1().ConfigMaps("default").Get(ctx, "bureau-web01-bureau", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if cm.Data["bureau.txt"] != "Hello world!" {
		t.Errorf("unexpected data: %v", cm.Data)
	}

	// update
	entry.Data = "Updated"
	entry.Mtime = "20240102000000Z"
	if err := ApplyConfigMap(client, "default", entry); err != nil {
		t.Fatalf("update: %v", err)
	}
	cm, _ = client.CoreV1().ConfigMaps("default").Get(ctx, "bureau-web01-bureau", metav1.GetOptions{})
	if cm.Data["bureau.txt"] != "Updated" || cm.Annotations[k8sAnnotationPrefix+"modifyTimestamp"] != entry.Mtime {
		t.Errorf("update not applied: %v %v", cm.Data, cm.Annotations)
	}

	// refuse to overwrite unmanaged ConfigMaps
	_, _ = client.CoreV1().ConfigMaps("default").Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "bureau-web01-foreign", Namespace: "default"},
	}, metav1.CreateOptions{})
	entry.DN = "cn=foreign,cn=web01,ou=config,dc=example,dc=com"
	if err := ApplyConfigMap(client, "default", entry); err == nil {
		t.Errorf("expected error when overwriting unmanaged ConfigMap")
	}

	// refuse to overwrite a ConfigMap owned by a different DN with the same name
	entry.DN = "cn=bureau,cn=web-01,ou=config,dc=example,dc=com" // -> bureau-web-01-bureau
	if err := ApplyConfigMap(client, "default", entry); err != nil {
		t.Fatalf("create second: %v", err)
	}
	entry.DN = "cn=bureau,cn=web--01,ou=config,dc=example,dc=com" // also -> bureau-web-01-bureau
	if err := ApplyConfigMap(client, "default", entry); err == nil {
		t.Errorf("expected error on ConfigMap name collision")
	}

	// non UTF-8 data goes to BinaryData
	entry.DN = "cn=bin,cn=web01,ou=config,dc=example,dc=com"
	entry.Data = "\xff\xfe"
	if err := ApplyConfigMap(client, "default", entry); err != nil {
		t.Fatalf("create binary: %v", err)
	}
	cm, _ = client.CoreV1().ConfigMaps("default").Get(ctx, "bureau-web01-bin", metav1.GetOptions{})
	if string(cm.BinaryData["bureau.txt"]) != entry.Data || len(cm.Data) != 0 {
		t.Errorf("binary data not stored in BinaryData: %v %v", cm.Data, cm.BinaryData)
	}
}
