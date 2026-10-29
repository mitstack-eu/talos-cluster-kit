package testutil

import (
	"cmp"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"
)

var applyPlaceRe = regexp.MustCompile(`^[0-9]+-`)

// ManifestName is the manifest a machine configuration names configurationName, without its place in the apply order.
func ManifestName(configurationName string) string {
	return applyPlaceRe.ReplaceAllString(configurationName, "")
}

// AppliedObject is one object of a manifest, Manifest being the manifest's name in the machine configuration.
type AppliedObject struct {
	Manifest string
	Doc      map[string]any
}

func (o AppliedObject) groupVersion() (string, string) {
	apiVersion, _ := o.Doc["apiVersion"].(string)
	group, version, found := strings.Cut(apiVersion, "/")
	if !found {
		return "", apiVersion
	}
	return group, version
}

func (o AppliedObject) kind() string { return fmt.Sprint(o.Doc["kind"]) }

func (o AppliedObject) meta(field string) string {
	value, _ := o.Doc["metadata"].(map[string]any)[field].(string)
	return value
}

func (o AppliedObject) String() string {
	name := o.meta("name")
	if ns := o.meta("namespace"); ns != "" {
		name = ns + "/" + name
	}
	return fmt.Sprintf("%s %s in %s", o.kind(), name, o.Manifest)
}

func (o AppliedObject) isNamespace() bool {
	_, version := o.groupVersion()
	return o.kind() == "Namespace" && version == "v1"
}

func (o AppliedObject) isCRD() bool {
	group, _ := o.groupVersion()
	return o.kind() == "CustomResourceDefinition" && group == "apiextensions.k8s.io"
}

// TalosOrder is the order Talos v1.14.4's ManifestApplyController creates the objects of manifests in, keyed by their
// names in the machine configuration: manifests by their id, priority 99 then the name, then Namespaces and CRDs first.
func TalosOrder(t *testing.T, manifests map[string][]byte) []AppliedObject {
	t.Helper()
	var objects []AppliedObject
	for _, name := range slices.SortedFunc(maps.Keys(manifests), func(a, b string) int { return cmp.Compare("99-"+a, "99-"+b) }) {
		for _, doc := range Documents(t, manifests[name]) {
			items, isList := doc["items"].([]any)
			if !isList {
				objects = append(objects, AppliedObject{Manifest: name, Doc: doc})
			}
			for _, item := range items {
				objects = append(objects, AppliedObject{Manifest: name, Doc: item.(map[string]any)})
			}
		}
	}
	sort.SliceStable(objects, func(i, j int) bool {
		l, r := objects[i], objects[j]
		switch {
		case l.isNamespace():
			return !r.isNamespace() || l.meta("name") < r.meta("name")
		case r.isNamespace():
			return false
		case l.isCRD():
			return !r.isCRD() || l.meta("name") < r.meta("name")
		default:
			return false
		}
	})
	return objects
}

// Webhook is one admission webhook of an applied ValidatingWebhookConfiguration or MutatingWebhookConfiguration.
type Webhook struct {
	Name          string
	Configuration AppliedObject
	// Service is the namespace/name of the Service the API server calls, empty for a webhook called at a URL.
	Service string
	rules   []webhookRule
}

type webhookRule struct {
	APIGroups   []string `yaml:"apiGroups"`
	APIVersions []string `yaml:"apiVersions"`
	Operations  []string `yaml:"operations"`
	Resources   []string `yaml:"resources"`
}

// Webhooks are every admission webhook objects configure, whether or not one that cannot answer refuses a create.
func Webhooks(t *testing.T, objects []AppliedObject) []Webhook {
	t.Helper()
	var webhooks []Webhook
	for _, o := range objects {
		if group, _ := o.groupVersion(); group != "admissionregistration.k8s.io" || !strings.HasSuffix(o.kind(), "WebhookConfiguration") {
			continue
		}
		for _, w := range o.Doc["webhooks"].([]any) {
			var hook struct {
				Name         string `yaml:"name"`
				ClientConfig struct {
					Service struct{ Namespace, Name string } `yaml:"service"`
				} `yaml:"clientConfig"`
				Rules []webhookRule `yaml:"rules"`
			}
			Decode(t, w, &hook)
			webhook := Webhook{Name: hook.Name, Configuration: o, rules: hook.Rules}
			if svc := hook.ClientConfig.Service; svc.Name != "" {
				webhook.Service = svc.Namespace + "/" + svc.Name
			}
			webhooks = append(webhooks, webhook)
		}
	}
	return webhooks
}

// Intercepts reports whether the API server calls w on creating o, judged by w's rules alone, its selectors aside.
func (w Webhook) Intercepts(o AppliedObject, plurals map[string]string) bool {
	group, version := o.groupVersion()
	resource, known := plurals[group+"/"+o.kind()]
	if !known {
		resource = strings.ToLower(o.kind()) + "s"
	}
	for _, r := range w.rules {
		if matchesAny(r.Operations, "CREATE") && matchesAny(r.APIGroups, group) && matchesAny(r.APIVersions, version) &&
			(matchesAny(r.Resources, resource) || slices.Contains(r.Resources, "*/*")) {
			return true
		}
	}
	return false
}

func matchesAny(values []string, want string) bool {
	return slices.Contains(values, "*") || slices.Contains(values, want)
}

// Plurals maps each custom resource's group/kind that objects define a CRD for to its plural, as a webhook rule names it.
func Plurals(t *testing.T, objects []AppliedObject) map[string]string {
	t.Helper()
	plurals := map[string]string{}
	for _, o := range objects {
		if !o.isCRD() {
			continue
		}
		var crd struct {
			Spec struct {
				Group string `yaml:"group"`
				Names struct{ Kind, Plural string }
			} `yaml:"spec"`
		}
		Decode(t, o.Doc, &crd)
		plurals[crd.Spec.Group+"/"+crd.Spec.Names.Kind] = crd.Spec.Names.Plural
	}
	return plurals
}

// Decode reads a decoded YAML node into out, through its YAML.
func Decode(t *testing.T, node any, out any) {
	t.Helper()
	data, err := yaml.Marshal(node)
	require.NoError(t, err)
	require.NoError(t, yaml.Unmarshal(data, out))
}
