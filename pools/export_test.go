package pools

func RenderWorkerJob(step, key, hostname string) ([]byte, error) {
	return workerJobSpec{step: step, key: key, hostname: hostname}.render()
}

func RenderIdleDrainJob(key, hostname string) ([]byte, error) {
	return workerJobSpec{step: drainStep, key: key, hostname: hostname, idleOnly: true}.render()
}

const (
	DrainStep      = drainStep
	RemoveNodeStep = removeNodeStep
)
