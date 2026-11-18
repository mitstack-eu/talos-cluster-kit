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

// Rerender renders the configuration this kit release gives the node whose configuration is running, from that node's
// own keys, cluster, addresses and name, keeping the manifests it applied at install since Argo CD carries each later
// change of a component. It refuses a release that changes a setting the node takes only at reboot.
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
	acceptAPIServerCA(bundle, cfg)
	c := Cluster{Profile: Metal, Secrets: &Secrets{bundle: bundle}, running: &runningDocuments{}}
	var n Node
	var labels map[string]string
	for _, doc := range cfg.Documents() {
		labels = c.readDocument(doc, &n, labels)
	}
	c.APIAddresses = append(c.APIAddresses, otherAPIAddresses(raw.MachineConfig.MachineCertSANs, c.APIAddresses)...)
	if n.Labels, err = c.ownLabels(role, labels); err != nil {
		return Cluster{}, Node{}, 0, err
	}
	return c, n, role, nil
}

// acceptAPIServerCA gives a worker's bundle the API server's CA it trusts, since a worker holds that CA without its key
// and Talos reads a bundle's CA only from a key pair.
func acceptAPIServerCA(bundle *secrets.Bundle, cfg machineryconfig.Provider) {
	ca := cfg.K8sAPIServerCAConfig()
	if bundle.Certs.K8s != nil || ca == nil || len(ca.AcceptedCAs()) == 0 {
		return
	}
	bundle.Certs.K8s = &x509.PEMEncodedCertificateAndKey{Crt: ca.AcceptedCAs()[0].Crt}
}

// readDocument takes what doc says of the cluster and the node, and returns the node's labels once doc states them.
func (c *Cluster) readDocument(doc config.Document, n *Node, labels map[string]string) map[string]string {
	switch d := doc.(type) {
	case *k8s.KubeClusterConfigV1Alpha1:
		c.Name = d.ClusterNameConfig
		c.APIAddresses = endpointAddress(d)
	case *k8s.KubeNodeConfigV1Alpha1:
		c.SubscriptionID = d.LabelsConfig[SubscriptionLabel]
		c.Network = firstPrefix(d.NodeIPConfig.NodeIPValidSubnets)
		c.AppsOnWorkers = len(d.TaintsConfig) > 0
		return d.LabelsConfig
	case *k8s.KubeletConfigV1Alpha1:
		c.Components.Versions.Kubernetes = strings.TrimPrefix(d.KubeletImage, constants.KubeletImage+":v")
	case *k8s.KubeServiceAccountConfigV1Alpha1:
		c.ServiceAccountIssuer = d.ServiceIssuer.IssuerURL.URL
	case *security.TrustedRootsConfigV1Alpha1:
		c.GatewayCA = []byte(d.Certificates)
	case *cri.RegistryTLSConfigV1Alpha1:
		if addr, err := netip.ParseAddr(d.MetaName); err == nil {
			c.Gateways = append(c.Gateways, addr)
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
		n.Hostname = d.ConfigHostname
	default:
		readVSwitch(doc, n)
	}
	return labels
}

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

func endpointAddress(d *k8s.KubeClusterConfigV1Alpha1) []netip.Addr {
	if d.ClusterEndpointConfig.URL == nil {
		return nil
	}
	addr, err := netip.ParseAddr(d.ClusterEndpointConfig.URL.Hostname())
	if err != nil {
		return nil
	}
	return []netip.Addr{addr}
}

func firstPrefix(subnets []string) netip.Prefix {
	if len(subnets) == 0 {
		return netip.Prefix{}
	}
	prefix, err := netip.ParsePrefix(subnets[0])
	if err != nil {
		return netip.Prefix{}
	}
	return prefix
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

// sameRebootOnlySettings refuses a rendered configuration that differs from the running one in a setting the node
// takes only at reboot, or in a system volume, which a node formats only once.
func sameRebootOnlySettings(running, rendered []byte) error {
	was, err := decodeDocuments(running)
	if err != nil {
		return fmt.Errorf("read the running configuration: %w", err)
	}
	is, err := decodeDocuments(rendered)
	if err != nil {
		return fmt.Errorf("read the rendered configuration: %w", err)
	}
	var changed []string
	for _, s := range rebootOnlySettings {
		if !reflect.DeepEqual(s.of(was), s.of(is)) {
			changed = append(changed, strings.Join(s.path, "."))
		}
	}
	for _, name := range systemVolumeNames(was, is) {
		if !reflect.DeepEqual(systemVolume(was, name), systemVolume(is, name)) {
			changed = append(changed, "VolumeConfig "+name)
		}
	}
	if changed != nil {
		return fmt.Errorf("the release changes %s: %w", strings.Join(changed, ", "), ErrRebootOnly)
	}
	return nil
}

func decodeDocuments(data []byte) ([]map[string]any, error) {
	var docs []map[string]any
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
		docs = append(docs, doc)
	}
}

// of is the setting as docs state it, nil where they state none.
func (s rebootOnly) of(docs []map[string]any) any {
	var node any
	for _, doc := range docs {
		if doc["version"] == "v1alpha1" && doc["kind"] == nil {
			node = doc
		}
	}
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

const volumeConfigKind = "VolumeConfig"

func systemVolumeNames(docs ...[]map[string]any) []string {
	var names []string
	for _, set := range docs {
		for _, doc := range set {
			name, _ := doc["name"].(string)
			if doc["kind"] == volumeConfigKind && !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
	}
	return names
}

func systemVolume(docs []map[string]any, name string) map[string]any {
	for _, doc := range docs {
		if doc["kind"] == volumeConfigKind && doc["name"] == name {
			return doc
		}
	}
	return nil
}
