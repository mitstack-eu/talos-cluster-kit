# Forty handover Jobs, each labelled as the kit's own.
{items: [range(40) | {kind: "Job", metadata: {namespace: "argocd", name: "component-\(.)-handover", labels: {"mitstack.dev/rendered-by": "talos-cluster-kit"}}}]}
