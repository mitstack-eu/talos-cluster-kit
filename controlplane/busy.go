package controlplane

import (
	"context"
	"fmt"
	"maps"
)

// attachToNetworkCommand is the command of the action Hetzner runs to attach a server to a network.
const attachToNetworkCommand = "attach_to_network"

// Busy names what the server is doing while Hetzner still runs an action on it, which locks it against another action,
// as "starting, since Hetzner still runs its start_server action on it"; it is empty once Hetzner runs none.
func (p Plane) Busy(ctx context.Context, server Server) (string, error) {
	command, err := p.API.RunningAction(ctx, server.ID)
	if err != nil || command == "" {
		return "", err
	}
	return fmt.Sprintf("%s, since Hetzner still runs its %s action on it", doing(command), command), nil
}

func doing(command string) string {
	switch command {
	case startServerCommand:
		return "starting"
	case StopServerCommand, ShutdownServerCommand:
		return "powering off"
	case attachToNetworkCommand:
		return "joining a network"
	}
	return "busy"
}

// Changed says how the server Hetzner holds now differs from its listed copy, as "running, though it was listed off",
// since an action that ends after the list changes the server the plane chose its step by; it is empty while the two agree.
func (p Plane) Changed(ctx context.Context, listed Server) (string, error) {
	now, found, err := p.API.GetServer(ctx, listed.ID)
	switch {
	case err != nil:
		return "", err
	case !found:
		return "gone, though it was listed " + listed.Status, nil
	case now.Status != listed.Status:
		return fmt.Sprintf("%s, though it was listed %s", now.Status, listed.Status), nil
	case !maps.Equal(now.PrivateIPs, listed.PrivateIPs):
		return "on other networks than it was listed on", nil
	}
	return "", nil
}

// waitOn names why the plane does not act on the listed server this call: what Hetzner still runs on it, or how it
// changed since the list. It reads the server again only once Hetzner runs nothing on it.
func (p Plane) waitOn(ctx context.Context, listed Server) (busy, changed string, err error) {
	if busy, err = p.Busy(ctx, listed); err != nil || busy != "" {
		return busy, "", err
	}
	changed, err = p.Changed(ctx, listed)
	return "", changed, err
}

// onceIdle takes step only once Hetzner runs no action on server and holds it as listed, since it refuses a second
// action at a locked server and the running one's outcome decides whether the step is still needed.
func (p Plane) onceIdle(ctx context.Context, server Server, step Step) (Step, bool, error) {
	busy, changed, err := p.waitOn(ctx, server)
	if err != nil || busy != "" || changed != "" {
		return Step{}, false, err
	}
	return step, true, nil
}

// stateOf says what a server that is not running is doing: its status, or what Hetzner still runs on it.
func (p Plane) stateOf(ctx context.Context, server Server) string {
	busy, err := p.Busy(ctx, server)
	switch {
	case err != nil:
		return fmt.Sprintf("%s, and what Hetzner runs on it cannot be read: %v", server.Status, err)
	case busy != "":
		return busy
	}
	return server.Status
}
