# Three failed pods of each of the forty Jobs. A kubelet keeps at most 4096 bytes of a termination message, so each
# message is 2048 two-byte characters.
{items: [range(40) as $j | range(3) | {
  metadata: {namespace: "argocd", name: "component-\($j)-handover-\(.)", creationTimestamp: "2026-11-13T10:3\(.):00Z",
    ownerReferences: [{kind: "Job", name: "component-\($j)-handover"}]},
  status: {phase: "Failed", containerStatuses: [{name: "handover", state: {terminated: {exitCode: 1, reason: "Error", message: ("é" * 2048)}}}]}
}]}
