package helpercontrol

import (
	"os"
	"strings"
	"time"

	"dedup/internal/nodectl"
)

type Inputs struct {
	MachineID      string
	ExecutablePath string
	ConfigSHA256   string
	StartedAt      time.Time
	DeleteService  interface {
		ActiveRequests() int
		Listening() bool
	}
}

type Provider struct{ inputs Inputs }

func NewProvider(inputs Inputs) *Provider { return &Provider{inputs: inputs} }

func (p *Provider) ControlStatus() nodectl.Status {
	listening := false
	active := 0
	if p.inputs.DeleteService != nil {
		listening = p.inputs.DeleteService.Listening()
		active = p.inputs.DeleteService.ActiveRequests()
		if active < 0 {
			active = 0
		}
	}
	lifecycle := "starting"
	if listening {
		lifecycle = "running"
	}
	return nodectl.Status{
		Component:       nodectl.ComponentHelper,
		MachineID:       p.inputs.MachineID,
		PID:             os.Getpid(),
		StartedAtUnixMS: p.inputs.StartedAt.UnixMilli(),
		ExecutablePath:  p.inputs.ExecutablePath,
		ConfigSHA256:    strings.ToLower(p.inputs.ConfigSHA256),
		Lifecycle:       lifecycle,
		ServiceReady:    listening,
		Ready:           listening,
		ActiveRequests:  active,
	}
}
