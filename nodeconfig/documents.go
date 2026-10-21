package nodeconfig

import (
	"fmt"
	"net/netip"
	"net/url"
	"slices"
	"strconv"

	"github.com/siderolabs/talos/pkg/machinery/config/config"
	"github.com/siderolabs/talos/pkg/machinery/config/types/cri"
	"github.com/siderolabs/talos/pkg/machinery/config/types/k8s"
	"github.com/siderolabs/talos/pkg/machinery/config/types/meta"
	"github.com/siderolabs/talos/pkg/machinery/config/types/network"
	"github.com/siderolabs/talos/pkg/machinery/config/types/runtime"
	"github.com/siderolabs/talos/pkg/machinery/config/types/security"
	"github.com/siderolabs/talos/pkg/machinery/config/types/v1alpha1"
	"github.com/siderolabs/talos/pkg/machinery/nethelpers"

	"github.com/mitstack-eu/talos-cluster-kit/components"
)

// HetznerResolvers are Hetzner's recursive resolvers, which every node asks in place of Talos's public defaults.
var HetznerResolvers = []netip.Addr{netip.MustParseAddr("185.12.64.1"), netip.MustParseAddr("185.12.64.2")}

// HetznerTimeServers are Hetzner's NTP servers, which every node syncs from in place of time.cloudflare.com.
var HetznerTimeServers = []string{"ntp1.hetzner.de", "ntp2.hetzner.com", "ntp3.hetzner.net"}

// TalosAPIRoles are the Talos roles a pod in TalosAPINamespace may hold: os:admin for the etcd member removal,
// which accepts no narrower role, os:etcd:backup for the snapshot, and os:reader for the node report.
var TalosAPIRoles = []string{"os:admin", "os:etcd:backup", "os:reader"}

// TalosAPINamespace is the one namespace whose pods may hold a Talos API identity.
const TalosAPINamespace = "mitstack-node"

// adapt changes the generated documents the kit keeps: no kube-proxy, since Cilium replaces it, the nodes'
// subscription label, etcd and the kubelet bound to the private network, and service logs sent to the
// node-local Collector when the cluster has a log receiver.
func (c Cluster) adapt(generated []config.Document) []config.Document {
	var docs []config.Document
	for _, doc := range generated {
		if !keepGenerated(doc) {
			continue
		}
		switch d := doc.(type) {
		case *v1alpha1.Config:
			c.adaptV1Alpha1(d)
		case *k8s.KubeProxyConfigV1Alpha1:
			d.ProxyEnabled = new(false)
		case *k8s.KubeNodeConfigV1Alpha1:
			d.LabelsConfig[SubscriptionLabel] = c.SubscriptionID
			d.NodeIPConfig.NodeIPValidSubnets = []string{c.Network.String()}
		}
		docs = append(docs, doc)
	}
	return docs
}

func (c Cluster) adaptV1Alpha1(d *v1alpha1.Config) {
	d.ClusterConfig.EtcdConfig.EtcdAdvertisedSubnets = []string{c.Network.String()}
	if c.Logs.IsZero() {
		return
	}
	d.MachineConfig.MachineLogging = &v1alpha1.LoggingConfig{
		LoggingDestinations: []v1alpha1.LoggingDestination{{
			LoggingEndpoint:  &v1alpha1.Endpoint{URL: loopbackLogURL(components.ServiceLogPort)},
			LoggingFormat:    "json_lines",
			LoggingExtraTags: map[string]string{"subscription": c.SubscriptionID},
		}},
	}
}

func loopbackLogURL(port int) *url.URL {
	return &url.URL{Scheme: "tcp", Host: "127.0.0.1:" + strconv.Itoa(port)}
}

// documents returns the documents the kit adds to the generated ones.
func (c Cluster) documents(n Node) ([]config.Document, error) {
	manifests, err := c.manifestDocuments()
	if err != nil {
		return nil, err
	}
	docs := []config.Document{resolverDocument(), timeSyncDocument(), hostnameDocument(n), c.trustedRootsDocument()}
	if !c.Logs.IsZero() {
		docs = append(docs, kernelLogDocument())
	}
	docs = append(docs, talosAPIAccessDocument())
	docs = append(docs, c.mirrorDocuments()...)
	return append(docs, manifests...), nil
}

func resolverDocument() config.Document {
	doc := network.NewResolverConfigV1Alpha1()
	for _, addr := range HetznerResolvers {
		doc.ResolverNameservers = append(doc.ResolverNameservers, network.NameserverConfig{Address: meta.Addr{Addr: addr}})
	}
	doc.ResolverHostDNS = network.HostDNSConfig{HostDNSEnabled: new(true), HostDNSForwardKubeDNSToHost: new(true)}
	return doc
}

func timeSyncDocument() config.Document {
	doc := network.NewTimeSyncConfigV1Alpha1()
	doc.TimeNTP = &network.NTPConfig{Servers: HetznerTimeServers}
	return doc
}

// hostnameDocument leaves a Cloud node's name to Hetzner's metadata service and gives a Metal node its own.
func hostnameDocument(n Node) config.Document {
	doc := network.NewHostnameConfigV1Alpha1()
	if n.Hostname == "" {
		doc.ConfigAuto = new(nethelpers.AutoHostnameKindOff)
		return doc
	}
	doc.ConfigHostname = n.Hostname
	return doc
}

func (c Cluster) trustedRootsDocument() config.Document {
	doc := security.NewTrustedRootsConfigV1Alpha1()
	doc.MetaName = "mitstack-gateway"
	doc.Certificates = string(c.GatewayCA)
	return doc
}

func kernelLogDocument() config.Document {
	doc := runtime.NewKmsgLogV1Alpha1()
	doc.MetaName = "node-local-collector"
	doc.KmsgLogURL = meta.URL{URL: loopbackLogURL(components.KernelLogPort)}
	return doc
}

func talosAPIAccessDocument() config.Document {
	doc := k8s.NewKubeTalosAPIAccessConfigV1Alpha1()
	doc.AccessAllowedRoles = TalosAPIRoles
	doc.AccessAllowedKubernetesNamespaces = []string{TalosAPINamespace}
	return doc
}

// mirrorDocuments send each public registry to every gateway's mirror in turn, with no fallback to the registry
// itself, and trust each gateway's certificate for those pulls.
func (c Cluster) mirrorDocuments() []config.Document {
	var docs []config.Document
	for _, registry := range components.MirroredRegistries {
		doc := cri.NewRegistryMirrorConfigV1Alpha1(registry)
		for _, gateway := range c.Gateways {
			doc.RegistryEndpoints = append(doc.RegistryEndpoints, cri.RegistryEndpoint{
				EndpointURL:          meta.URL{URL: &url.URL{Scheme: "https", Host: gateway.String(), Path: "/v2/" + registry}},
				EndpointOverridePath: new(true),
			})
		}
		doc.RegistrySkipFallback = new(true)
		docs = append(docs, doc)
	}
	for _, gateway := range c.Gateways {
		doc := cri.NewRegistryTLSConfigV1Alpha1(gateway.String())
		doc.TLSCA = string(c.GatewayCA)
		docs = append(docs, doc)
	}
	return docs
}

// manifestDocuments name each served manifest at the first gateway, and carry each inline one with the
// node-local Collector when the cluster has a log receiver.
func (c Cluster) manifestDocuments() ([]config.Document, error) {
	inline := c.Components.Inline
	if !c.Logs.IsZero() {
		collector, err := components.LogCollector(c.Logs)
		if err != nil {
			return nil, fmt.Errorf("cluster %q: %w", c.Name, err)
		}
		inline = append(slices.Clone(inline), collector)
	}
	var docs []config.Document
	for _, m := range c.Components.Served {
		doc := k8s.NewKubeExternalManifestConfigV1Alpha1()
		doc.MetaName = m.Name
		doc.URLSpec = meta.URL{URL: ManifestURL(c.Gateways[0], m)}
		docs = append(docs, doc)
	}
	for _, m := range inline {
		doc := k8s.NewKubeInlineManifestConfigV1Alpha1()
		doc.MetaName = m.Name
		doc.ManifestSpec = string(m.Bytes)
		docs = append(docs, doc)
	}
	return docs, nil
}

// ManifestURL is where gateway serves a served manifest.
func ManifestURL(gateway netip.Addr, m components.Manifest) *url.URL {
	return &url.URL{Scheme: "https", Host: gateway.String(), Path: "/manifests/" + m.Name + ".yaml"}
}
