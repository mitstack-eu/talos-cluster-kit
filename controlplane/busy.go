package controlplane

import (
	"context"
	"fmt"
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

// onceIdle takes step only once Hetzner runs no action on server, since it refuses a second action at a locked server
// and the running one's outcome decides whether the step is still needed.
func (p Plane) onceIdle(ctx context.Context, server Server, step Step) (Step, bool, error) {
	busy, err := p.Busy(ctx, server)
	if err != nil || busy != "" {
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
