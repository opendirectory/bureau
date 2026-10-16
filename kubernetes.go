package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-ldap/ldap/v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	k8sAnnotationPrefix = "bureau.slapcat.github.com/"
	k8sManagedByLabel   = "app.kubernetes.io/managed-by"
	k8sManagedByValue   = "bureau"
	k8sDefaultPrefix    = "bureau"
	k8sDefaultKey       = "data"
	k8sMaxNameLen       = 253
	k8sRequestTimeout   = 30 * time.Second
)

// ConfigMapEntry holds the full rendered content of a synced file that
// should be published to Kubernetes as a ConfigMap. When several LDAP
// entries target the same path, DN is the first of them and Data is their
// concatenated output.
type ConfigMapEntry struct {
	DN    string
	Path  string
	Data  string
	Perm  string
	Mtime string
}

var (
	// ConfigMaps is keyed by file path. Entries that fail to publish stay
	// queued and are retried on the next sync (or replaced by newer data).
	ConfigMaps   map[string]ConfigMapEntry
	k8sClient    kubernetes.Interface
	k8sNamespace string

	invalidNameChars = regexp.MustCompile(`[^a-z0-9.-]+`)
	invalidKeyChars  = regexp.MustCompile(`[^-._a-zA-Z0-9]+`)
	repeatedDashes   = regexp.MustCompile(`-{2,}`)
)

// QueueConfigMap records a synced file so it can be published as a
// ConfigMap once the sync has finished. It is a no-op unless Kubernetes
// support is enabled.
func QueueConfigMap(entry ConfigMapEntry) {
	if !C.Kubernetes.Enabled {
		return
	}

	if ConfigMaps == nil {
		ConfigMaps = make(map[string]ConfigMapEntry)
	}

	ConfigMaps[entry.Path] = entry
}

// Overridable in tests
var (
	inClusterConfig             = rest.InClusterConfig
	serviceAccountNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
)

// K8sRestConfig returns the client configuration and the namespace to use.
//
// By default the pod's service account credentials are used (in-cluster
// config). $KUBECONFIG and ~/.kube/config are deliberately ignored, so a
// stray kubeconfig can't redirect bureau to another cluster; a kubeconfig
// is only used if its path is set explicitly in the bureau config.
func K8sRestConfig() (*rest.Config, string, error) {
	namespace := C.Kubernetes.Namespace

	if C.Kubernetes.Kubeconfig != "" {
		rules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: ExpandPath(C.Kubernetes.Kubeconfig)}
		clientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{})

		restConfig, err := clientConfig.ClientConfig()
		if err != nil {
			return nil, "", fmt.Errorf("loading kubeconfig %s: %w", rules.ExplicitPath, err)
		}

		if namespace == "" {
			if ns, _, err := clientConfig.Namespace(); err == nil && ns != "" {
				namespace = ns
			}
		}
		if namespace == "" {
			namespace = metav1.NamespaceDefault
		}

		return restConfig, namespace, nil
	}

	restConfig, err := inClusterConfig()
	if err != nil {
		return nil, "", fmt.Errorf("loading in-cluster service account config (set kubernetes.kubeconfig when running outside a pod): %w", err)
	}

	if namespace == "" {
		data, err := os.ReadFile(serviceAccountNamespaceFile)
		if err != nil {
			return nil, "", fmt.Errorf("reading pod namespace (set kubernetes.namespace): %w", err)
		}
		namespace = strings.TrimSpace(string(data))
	}
	if namespace == "" {
		return nil, "", fmt.Errorf("pod namespace is empty (set kubernetes.namespace)")
	}

	return restConfig, namespace, nil
}

// K8sConnect lazily builds a Kubernetes client (see K8sRestConfig).
func K8sConnect() (kubernetes.Interface, string, error) {
	if k8sClient != nil {
		return k8sClient, k8sNamespace, nil
	}

	restConfig, namespace, err := K8sRestConfig()
	if err != nil {
		return nil, "", err
	}

	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, "", fmt.Errorf("creating kubernetes client: %w", err)
	}

	k8sClient = client
	k8sNamespace = namespace
	Logger(nil, "Connected to kubernetes, using namespace: "+namespace, "DEBUG")

	return k8sClient, k8sNamespace, nil
}

// PublishConfigMaps creates or updates a ConfigMap for every queued entry.
func PublishConfigMaps() error {
	if !C.Kubernetes.Enabled || len(ConfigMaps) == 0 {
		return nil
	}

	client, namespace, err := K8sConnect()
	if err != nil {
		return err
	}

	failed := 0
	for path, entry := range ConfigMaps {
		err := ApplyConfigMap(client, namespace, entry)
		if err != nil {
			Logger(err, "Error publishing ConfigMap for "+entry.DN, "WARN")
			failed++
			// keep the entry queued so it is retried on the next sync
			continue
		}
		delete(ConfigMaps, path)
	}

	if failed > 0 {
		return fmt.Errorf("failed to publish %d ConfigMap(s)", failed)
	}

	return nil
}

// ApplyConfigMap creates the ConfigMap for an entry, or updates it if it
// already exists and is managed by bureau.
func ApplyConfigMap(client kubernetes.Interface, namespace string, entry ConfigMapEntry) error {
	name := ConfigMapName(entry.DN)
	key := ConfigMapKey(entry.Path)

	labels := map[string]string{
		k8sManagedByLabel: k8sManagedByValue,
	}
	annotations := map[string]string{
		k8sAnnotationPrefix + "dn":              entry.DN,
		k8sAnnotationPrefix + "path":            entry.Path,
		k8sAnnotationPrefix + "permissions":     entry.Perm,
		k8sAnnotationPrefix + "modifyTimestamp": entry.Mtime,
	}
	// ConfigMap Data must be UTF-8; anything else has to go in BinaryData
	var data map[string]string
	var binaryData map[string][]byte
	if utf8.ValidString(entry.Data) {
		data = map[string]string{key: entry.Data}
	} else {
		binaryData = map[string][]byte{key: []byte(entry.Data)}
	}

	ctx, cancel := context.WithTimeout(context.Background(), k8sRequestTimeout)
	defer cancel()

	cms := client.CoreV1().ConfigMaps(namespace)

	existing, err := cms.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:        name,
				Namespace:   namespace,
				Labels:      labels,
				Annotations: annotations,
			},
			Data:       data,
			BinaryData: binaryData,
		}

		_, err = cms.Create(ctx, cm, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("creating ConfigMap %s/%s: %w", namespace, name, err)
		}

		Logger(nil, "Created ConfigMap "+namespace+"/"+name+" from "+entry.DN, "DEBUG")
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting ConfigMap %s/%s: %w", namespace, name, err)
	}

	// Never clobber ConfigMaps that bureau did not create
	if existing.Labels[k8sManagedByLabel] != k8sManagedByValue {
		return fmt.Errorf("ConfigMap %s/%s exists and is not managed by bureau", namespace, name)
	}

	// Different DNs can sanitize to the same name; don't let them overwrite
	// each other
	if owner := existing.Annotations[k8sAnnotationPrefix+"dn"]; owner != "" && !strings.EqualFold(owner, entry.DN) {
		return fmt.Errorf("ConfigMap %s/%s already belongs to %s", namespace, name, owner)
	}

	// Skip the update if nothing changed
	upToDate := reflect.DeepEqual(existing.Data, data) && reflect.DeepEqual(existing.BinaryData, binaryData)
	for k, v := range annotations {
		upToDate = upToDate && existing.Annotations[k] == v
	}
	if upToDate {
		Logger(nil, "ConfigMap "+namespace+"/"+name+" is up to date", "DEBUG")
		return nil
	}

	updated := existing.DeepCopy()
	if updated.Labels == nil {
		updated.Labels = map[string]string{}
	}
	for k, v := range labels {
		updated.Labels[k] = v
	}
	if updated.Annotations == nil {
		updated.Annotations = map[string]string{}
	}
	for k, v := range annotations {
		updated.Annotations[k] = v
	}
	updated.Data = data
	updated.BinaryData = binaryData

	_, err = cms.Update(ctx, updated, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("updating ConfigMap %s/%s: %w", namespace, name, err)
	}

	Logger(nil, "Updated ConfigMap "+namespace+"/"+name+" from "+entry.DN, "DEBUG")
	return nil
}

// ConfigMapName derives a valid, deterministic ConfigMap name (RFC 1123
// subdomain) from an LDAP DN. The RDN values below the configured base are
// joined from the top down, e.g. with base "ou=config,dc=example,dc=com":
//
//	cn=bureau,cn=web01,ou=config,dc=example,dc=com -> bureau-web01-bureau
func ConfigMapName(dn string) string {
	prefix := C.Kubernetes.Prefix
	if prefix == "" {
		prefix = k8sDefaultPrefix
	}

	parts := []string{prefix}
	for _, value := range relativeRDNValues(dn, C.Base) {
		parts = append(parts, value)
	}

	name := strings.ToLower(strings.Join(parts, "-"))
	name = invalidNameChars.ReplaceAllString(name, "-")

	// Every dot-separated label must start and end with an alphanumeric
	// (e.g. "cn=.bashrc" would otherwise give "bureau-web01-.bashrc")
	var labels []string
	for _, label := range strings.Split(name, ".") {
		label = repeatedDashes.ReplaceAllString(label, "-")
		label = strings.Trim(label, "-")
		if label != "" {
			labels = append(labels, label)
		}
	}
	name = strings.Join(labels, ".")

	// Keep the name unique if it has to be truncated
	if len(name) > k8sMaxNameLen {
		sum := sha256.Sum256([]byte(dn))
		suffix := hex.EncodeToString(sum[:])[:8]
		name = strings.TrimRight(name[:k8sMaxNameLen-len(suffix)-1], "-.") + "-" + suffix
	}

	if name == "" {
		name = k8sDefaultPrefix
	}

	return name
}

// ConfigMapKey derives a valid ConfigMap data key from the target file
// path. Both "/" and "\" are treated as separators, so Windows paths give
// the same key regardless of which OS bureau runs on.
func ConfigMapKey(path string) string {
	base := strings.TrimRight(path, `/\`)
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	// drop a drive letter such as "C:"
	if len(base) == 2 && base[1] == ':' {
		base = ""
	}

	key := invalidKeyChars.ReplaceAllString(base, "_")

	if key == "" || key == "." || key == ".." || key == "/" || key == "_" {
		return k8sDefaultKey
	}
	if len(key) > k8sMaxNameLen {
		key = key[:k8sMaxNameLen]
	}

	return key
}

// relativeRDNValues returns the RDN values of dn that sit below base,
// ordered from the top of the tree downwards. If dn is not under base, all
// RDN values of dn are returned.
func relativeRDNValues(dn, base string) []string {
	parsedDN, err := ldap.ParseDN(dn)
	if err != nil {
		return []string{dn}
	}

	rdns := parsedDN.RDNs
	if parsedBase, err := ldap.ParseDN(base); err == nil && parsedBase.AncestorOf(parsedDN) {
		rdns = rdns[:len(rdns)-len(parsedBase.RDNs)]
	}

	values := make([]string, 0, len(rdns))
	for i := len(rdns) - 1; i >= 0; i-- {
		for _, attr := range rdns[i].Attributes {
			values = append(values, attr.Value)
		}
	}

	return values
}
