package nodeconfig

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/netip"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/siderolabs/crypto/x509"
	machineryconfig "github.com/siderolabs/talos/pkg/machinery/config"
	"github.com/siderolabs/talos/pkg/machinery/config/config"
	"github.com/siderolabs/talos/pkg/machinery/config/configloader"
	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
	"github.com/siderolabs/talos/pkg/machinery/config/machine"
	"github.com/siderolabs/talos/pkg/machinery/config/types/block"
	"github.com/siderolabs/talos/pkg/machinery/config/types/cri"
	"github.com/siderolabs/talos/pkg/machinery/config/types/k8s"
	"github.com/siderolabs/talos/pkg/machinery/config/types/network"
	"github.com/siderolabs/talos/pkg/machinery/config/types/runtime"
	"github.com/siderolabs/talos/pkg/machinery/config/types/security"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"go.yaml.in/yaml/v4"
)

// MachineConfigAnnotation names, on each node, the release its machine configuration was rendered from.
const MachineConfigAnnotation = "mitstack.dev/machine-config"

// ErrRebootOnly refuses a configuration that changes a setting a running node takes only when it reboots.
var ErrRebootOnly = errors.New("a node takes this setting only when it reboots")

// Rerender renders the configuration this kit release gives the running node from its own keys, cluster, addresses
// and name, keeping the manifests it applied at install since Argo CD carries each later change of a component.
func Rerender(running []byte, release string) ([]byte, error) {
	if release == "" {
		return nil, errors.New("rerender names no release for the node's annotation")
	}
	cfg, err := configloader.NewFromBytes(running)
	if err != nil {
		return nil, fmt.Errorf("read the running configuration: %w", err)
	}
	cluster, node, role, err := readRunning(cfg)
	if err != nil {
		return nil, err
	}
	cluster.Release = release
	rendered, err := cluster.render(role, node)
	if err != nil {
		return nil, err
	}
	if err := sameRebootOnlySettings(running, rendered); err != nil {
		return nil, err
	}
	return rendered, nil
}

// readRunning reads back the cluster and node a configuration was rendered for, and the node's role.
func readRunning(cfg machineryconfig.Provider) (Cluster, Node, machine.Type, error) {
	raw := cfg.RawV1Alpha1()
	if raw == nil || raw.MachineConfig == nil {
		return Cluster{}, Node{}, 0, errors.New("the running configuration holds no v1alpha1 document with its machine")
	}
	role, err := machine.ParseType(raw.MachineConfig.MachineType)
	if err != nil {
		return Cluster{}, Node{}, 0, fmt.Errorf("the running configuration's machine type: %w", err)
	}
	bundle, err := secrets.NewBundleFromConfig(secrets.NewClock(), cfg)
	if err != nil {
		return Cluster{}, Node{}, 0, fmt.Errorf("read the running configuration's secrets: %w", err)
	}
	if err := acceptAPIServerCA(bundle, cfg); err != nil {
		return Cluster{}, Node{}, 0, err
	}
	r := reader{Cluster: Cluster{Profile: Metal, Secrets: &Secrets{bundle: bundle}, running: &runningDocuments{}}}
	for _, doc := range cfg.Documents() {
		if err := r.read(doc); err != nil {
			return Cluster{}, Node{}, 0, fmt.Errorf("read the running configuration: %w", err)
		}
	}
	if err := r.complete(); err != nil {
		return Cluster{}, Node{}, 0, fmt.Errorf("read the running configuration: %w", err)
	}
	c := r.Cluster
	c.APIAddresses = append(c.APIAddresses, otherAPIAddresses(raw.MachineConfig.MachineCertSANs, c.APIAddresses)...)
	n := r.node
	if n.Labels, err = c.ownLabels(role, r.labels); err != nil {
		return Cluster{}, Node{}, 0, err
	}
	return c, n, role, nil
}

// acceptAPIServerCA gives a worker's bundle the API server's CA it trusts, since a worker holds that CA without its key
// and Talos reads a bundle's CA only from a key pair.
func acceptAPIServerCA(bundle *secrets.Bundle, cfg machineryconfig.Provider) error {
	if bundle.Certs.K8s != nil {
		return nil
	}
	ca := cfg.K8sAPIServerCAConfig()
	if ca == nil || len(ca.AcceptedCAs()) == 0 {
		return errors.New("the running configuration trusts no API server CA")
	}
	if n := len(ca.AcceptedCAs()); n > 1 {
		return fmt.Errorf("the running configuration trusts %d API server CAs, and a rendered worker trusts one", n)
	}
	bundle.Certs.K8s = &x509.PEMEncodedCertificateAndKey{Crt: ca.AcceptedCAs()[0].Crt}
	return nil
}

// reader gathers the cluster and node a running configuration's documents state.
type reader struct {
	Cluster
	node        Node
	labels      map[string]string
	registries  []registryTLS
	clusterSeen bool
	nodeSeen    bool
}

// registryTLS is a registry the node reaches over TLS, which is a gateway when it trusts the gateways' CA.
type registryTLS struct {
	addr netip.Addr
	ca   string
}

// read takes what doc says of the cluster and the node.
func (r *reader) read(doc config.Document) error {
	c := &r.Cluster
	switch d := doc.(type) {
	case *k8s.KubeClusterConfigV1Alpha1:
		r.clusterSeen = true
		c.Name = d.ClusterNameConfig
		addr, err := endpointAddress(d)
		if err != nil {
			return err
		}
		c.APIAddresses = []netip.Addr{addr}
	case *k8s.KubeNodeConfigV1Alpha1:
		r.nodeSeen = true
		c.SubscriptionID = d.LabelsConfig[SubscriptionLabel]
		r.labels = d.LabelsConfig
		for taint := range d.TaintsConfig {
			if taint != constants.LabelNodeRoleControlPlane {
				return fmt.Errorf("the node runs the taint %s, which the kit renders on no node", taint)
			}
			c.AppsOnWorkers = true
		}
		if len(d.NodeIPConfig.NodeIPValidSubnets) == 0 {
			return nil
		}
		prefix, err := netip.ParsePrefix(d.NodeIPConfig.NodeIPValidSubnets[0])
		if err != nil {
			return fmt.Errorf("the node's private network: %w", err)
		}
		c.Network = prefix
	case *k8s.KubeletConfigV1Alpha1:
		version, ok := strings.CutPrefix(d.KubeletImage, constants.KubeletImage+":v")
		if !ok || !kubernetesVersion.MatchString(version) {
			return fmt.Errorf("the kubelet image %s is no %s release", d.KubeletImage, constants.KubeletImage)
		}
		c.Components.Versions.Kubernetes = version
	case *k8s.KubeServiceAccountConfigV1Alpha1:
		c.ServiceAccountIssuer = d.ServiceIssuer.IssuerURL.URL
	case *security.TrustedRootsConfigV1Alpha1:
		if d.MetaName == gatewayRoots {
			c.GatewayCA = []byte(d.Certificates)
		}
	case *cri.RegistryTLSConfigV1Alpha1:
		if addr, err := netip.ParseAddr(d.MetaName); err == nil {
			r.registries = append(r.registries, registryTLS{addr: addr, ca: d.TLSCA})
		}
	case *network.LinkConfigV1Alpha1:
		if d.MetaName == cloudLink {
			c.Profile = HCloud
		}
	case *runtime.KmsgLogV1Alpha1:
		c.running.logs = true
	case *block.UserVolumeConfigV1Alpha1:
		c.Components.UserVolumes = append(c.Components.UserVolumes, d.MetaName)
	case *k8s.KubeInlineManifestConfigV1Alpha1, *k8s.KubeExternalManifestConfigV1Alpha1:
		c.running.manifests = append(c.running.manifests, doc)
	case *network.HostnameConfigV1Alpha1:
		r.node.Hostname = d.ConfigHostname
	default:
		readVSwitch(doc, &r.node)
	}
	return nil
}

// complete refuses a configuration missing a document that names the cluster or the node, and takes as gateways the
// registries that trust the gateways' CA, leaving any other for keepsEveryDocument to name.
func (r *reader) complete() error {
	switch {
	case !r.clusterSeen:
		return errors.New("it holds no KubeClusterConfig document naming the cluster")
	case !r.nodeSeen:
		return errors.New("it holds no KubeNodeConfig document naming the node's subscription")
	}
	for _, registry := range r.registries {
		if registry.ca == string(r.GatewayCA) {
			r.Gateways = append(r.Gateways, registry.addr)
		}
	}
	return nil
}

var kubernetesVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// otherAPIAddresses are the API addresses the node's certificate names beside the endpoint, in the order they were given.
func otherAPIAddresses(sans []string, endpoint []netip.Addr) []netip.Addr {
	var addrs []netip.Addr
	for _, san := range sans {
		addr, err := netip.ParseAddr(san)
		if err == nil && !slices.Contains(endpoint, addr) {
			addrs = append(addrs, addr)
		}
	}
	return addrs
}

func endpointAddress(d *k8s.KubeClusterConfigV1Alpha1) (netip.Addr, error) {
	if d.ClusterEndpointConfig.URL == nil {
		return netip.Addr{}, errors.New("the cluster names no API endpoint")
	}
	addr, err := netip.ParseAddr(d.ClusterEndpointConfig.URL.Hostname())
	if err != nil {
		return netip.Addr{}, fmt.Errorf("the cluster's API endpoint %s names no address: %w", d.ClusterEndpointConfig.URL, err)
	}
	return addr, nil
}

var linkMAC = regexp.MustCompile(`^mac\(link\.permanent_addr\) == "([0-9a-f:]+)"$`)

// readVSwitch takes a dedicated node's vSwitch from the card alias and the VLAN vswitchDocuments render.
func readVSwitch(doc config.Document, n *Node) {
	switch d := doc.(type) {
	case *network.LinkAliasConfigV1Alpha1:
		match := linkMAC.FindStringSubmatch(d.Selector.Match.String())
		if d.MetaName != publicLink || match == nil {
			return
		}
		mac, err := net.ParseMAC(match[1])
		if err != nil {
			return
		}
		vswitch(n).MAC = mac
	case *network.VLANConfigV1Alpha1:
		if d.MetaName != vswitchLink || len(d.LinkAddresses) == 0 {
			return
		}
		vswitch(n).VLAN = d.VLANIDConfig
		vswitch(n).Address = d.LinkAddresses[0].AddressAddress
	}
}

func vswitch(n *Node) *VSwitch {
	if n.VSwitch == nil {
		n.VSwitch = &VSwitch{}
	}
	return n.VSwitch
}

// ownLabels are the labels the node was given beside the subscription's and those Talos generates for its role.
func (c Cluster) ownLabels(role machine.Type, labels map[string]string) (map[string]string, error) {
	generated, err := c.generate(role)
	if err != nil {
		return nil, err
	}
	own := maps.Clone(labels)
	delete(own, SubscriptionLabel)
	for _, doc := range generated.Documents() {
		if d, ok := doc.(*k8s.KubeNodeConfigV1Alpha1); ok {
			for key := range d.LabelsConfig {
				delete(own, key)
			}
		}
	}
	if len(own) == 0 {
		return nil, nil
	}
	return own, nil
}

// rebootOnly is a setting of the v1alpha1 document a running node takes only at its next boot, at its path there,
// with the keys under it a node takes at once.
type rebootOnly struct {
	path   []string
	except []string
}

var rebootOnlySettings = []rebootOnly{
	{path: []string{"machine", "install"}},
	{path: []string{"machine", "kernel"}},
	// etcd's CA is a key, which the node keeps, so only the settings around it count.
	{path: []string{"cluster", "etcd"}, except: []string{"ca"}},
}

// sameRebootOnlySettings refuses a rendered configuration that drops a document the node runs, or differs from the
// running one in a setting the node takes only at reboot or in a volume, which a node formats only once.
func sameRebootOnlySettings(running, rendered []byte) error {
	was, err := decodeDocuments(running)
	if err != nil {
		return fmt.Errorf("read the running configuration: %w", err)
	}
	is, err := decodeDocuments(rendered)
	if err != nil {
		return fmt.Errorf("read the rendered configuration: %w", err)
	}
	if err := keepsEveryDocument(was, is); err != nil {
		return err
	}
	wasMachine, err := machineDocument(was)
	if err != nil {
		return fmt.Errorf("read the running configuration: %w", err)
	}
	isMachine, err := machineDocument(is)
	if err != nil {
		return fmt.Errorf("read the rendered configuration: %w", err)
	}
	var changed []string
	for _, s := range rebootOnlySettings {
		if !reflect.DeepEqual(s.of(wasMachine), s.of(isMachine)) {
			changed = append(changed, strings.Join(s.path, "."))
		}
	}
	for _, key := range volumeKeys(was, is) {
		if !reflect.DeepEqual(was[key], is[key]) {
			changed = append(changed, key.kind+" "+key.name)
		}
	}
	if changed != nil {
		return fmt.Errorf("the release changes %s: %w", strings.Join(changed, ", "), ErrRebootOnly)
	}
	return nil
}

// documentKey names a document of a configuration, the v1alpha1 one by an empty kind.
type documentKey struct{ kind, name string }

func decodeDocuments(data []byte) (map[documentKey]map[string]any, error) {
	docs := map[documentKey]map[string]any{}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var doc map[string]any
		err := decoder.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return docs, nil
		}
		if err != nil {
			return nil, err
		}
		kind, _ := doc["kind"].(string)
		name, _ := doc["name"].(string)
		key := documentKey{kind, name}
		if _, twice := docs[key]; twice {
			return nil, fmt.Errorf("it holds two %s documents named %q", kindName(kind), name)
		}
		docs[key] = doc
	}
}

func kindName(kind string) string {
	if kind == "" {
		return "v1alpha1"
	}
	return kind
}

// keepsEveryDocument refuses a rendered configuration missing a document the node runs, which nothing else would
// name before the node lost it.
func keepsEveryDocument(was, is map[documentKey]map[string]any) error {
	var dropped []string
	for key := range was {
		if _, ok := is[key]; !ok {
			dropped = append(dropped, strings.TrimSpace(kindName(key.kind)+" "+key.name))
		}
	}
	if dropped == nil {
		return nil
	}
	slices.Sort(dropped)
	return fmt.Errorf("the release renders no %s the node runs", strings.Join(dropped, ", "))
}

func machineDocument(docs map[documentKey]map[string]any) (map[string]any, error) {
	doc, ok := docs[documentKey{}]
	if !ok || doc["version"] != "v1alpha1" {
		return nil, errors.New("it holds no v1alpha1 document")
	}
	return doc, nil
}

// of is the setting as doc states it, nil where it states none.
func (s rebootOnly) of(doc map[string]any) any {
	var node any = doc
	for _, step := range s.path {
		m, ok := node.(map[string]any)
		if !ok {
			return nil
		}
		node = m[step]
	}
	m, ok := node.(map[string]any)
	if !ok {
		return node
	}
	m = maps.Clone(m)
	for _, key := range s.except {
		delete(m, key)
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// volumeKinds are the documents that state a volume a node formats once.
var volumeKinds = []string{"VolumeConfig", "UserVolumeConfig"}

func volumeKeys(docs ...map[documentKey]map[string]any) []documentKey {
	var keys []documentKey
	for _, set := range docs {
		for key := range set {
			if slices.Contains(volumeKinds, key.kind) && !slices.Contains(keys, key) {
				keys = append(keys, key)
			}
		}
	}
	slices.SortFunc(keys, func(a, b documentKey) int { return strings.Compare(a.kind+" "+a.name, b.kind+" "+b.name) })
	return keys
}
