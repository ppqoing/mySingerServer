package production

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"

	"dedup/internal/localcontrol"
	"dedup/internal/machineid"
	"dedup/internal/nodectl"
	"dedup/internal/nodetray/agentclient"
	trayconfig "dedup/internal/nodetray/config"
	"dedup/internal/nodetray/traymodel"
)

type FormValidationStore interface {
	ValidateAgentForm(trayconfig.AgentForm) []trayconfig.FieldError
	ValidateHelperForm(trayconfig.HelperForm) []trayconfig.FieldError
}

type Validator struct{ store FormValidationStore }

func NewValidator(store FormValidationStore) *Validator { return &Validator{store: store} }

func (v *Validator) ValidateAgent(value trayconfig.AgentForm) []trayconfig.FieldError {
	if v == nil || v.store == nil {
		return []trayconfig.FieldError{{Field: "agent", Code: "unavailable", Message: "Agent 配置验证不可用"}}
	}
	return append([]trayconfig.FieldError(nil), v.store.ValidateAgentForm(value)...)
}

func (v *Validator) ValidateHelper(value trayconfig.HelperForm) []trayconfig.FieldError {
	if v == nil || v.store == nil {
		return []trayconfig.FieldError{{Field: "helper", Code: "unavailable", Message: "Helper 配置验证不可用"}}
	}
	return append([]trayconfig.FieldError(nil), v.store.ValidateHelperForm(value)...)
}

type Dialer interface {
	Dial(context.Context, string) (net.Conn, error)
}

type FixedController struct {
	dialer    Dialer
	pipeName  string
	component nodectl.Component
	machineID string
}

type AgentConnectionSource func(context.Context) (configuredEndpoint, token string, err error)

func NewAgentController(_ Dialer, machineID string, sources ...AgentConnectionSource) (*agentclient.Controller, error) {
	if !validMachineID(machineID) || len(sources) > 1 {
		return nil, errors.New("production controller: fixed identity unavailable")
	}
	source := defaultAgentConnectionSource
	if len(sources) == 1 {
		source = sources[0]
	}
	if source == nil {
		return nil, errors.New("production controller: Agent connection unavailable")
	}
	endpoint, token, err := source(context.Background())
	if err != nil {
		return nil, errors.New("production controller: Agent connection unavailable")
	}
	controller, err := agentclient.NewController(endpoint, token, machineID)
	if err != nil {
		return nil, errors.New("production controller: Agent connection unavailable")
	}
	return controller, nil
}

func NewHelperController(dialer Dialer, machineID string) (*FixedController, error) {
	return newFixedController(dialer, nodectl.HelperPipeName(), nodectl.ComponentHelper, machineID)
}

func defaultAgentConnectionSource(context.Context) (string, string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", "", err
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return "", "", err
	}
	layout, err := ResolvePortableLayout(executable)
	if err != nil {
		return "", "", err
	}
	store, err := trayconfig.NewStore(trayconfig.Paths{
		TraySettings: layout.TraySettings, AgentConfig: layout.AgentConfig, HelperConfig: layout.HelperConfig,
		AgentExecutable: layout.AgentExecutable, HelperExecutable: layout.HelperExecutable,
	})
	if err != nil {
		return "", "", err
	}
	form, err := store.LoadAgentForm()
	if err != nil {
		return "", "", err
	}
	token, err := (localcontrol.FileTokenStore{}).LoadOrCreate(localcontrol.TokenPath(layout.Root))
	if err != nil {
		return "", "", err
	}
	return net.JoinHostPort(form.ListenHost, strconv.Itoa(form.ListenPort)), token, nil
}

func newFixedController(dialer Dialer, pipeName string, component nodectl.Component, machineID string) (*FixedController, error) {
	if dialer == nil || !validMachineID(machineID) {
		return nil, errors.New("production controller: fixed identity unavailable")
	}
	return &FixedController{dialer: dialer, pipeName: pipeName, component: component, machineID: machineID}, nil
}

func (c *FixedController) Status(ctx context.Context) (nodectl.Status, error) {
	if c == nil || c.dialer == nil || c.pipeName == "" {
		return nodectl.Status{}, errors.New("production controller: unavailable")
	}
	machineID := c.machineID
	if !validMachineID(machineID) {
		return nodectl.Status{}, errors.New("production controller: unavailable")
	}
	client := nodectl.NewClient(func(ctx context.Context) (net.Conn, error) {
		return c.dialer.Dial(ctx, c.pipeName)
	})
	status, err := client.Status(ctx)
	if err != nil {
		return nodectl.Status{}, errors.New("production controller: status unavailable")
	}
	if err := status.Validate(); err != nil {
		return nodectl.Status{}, errors.New("production controller: invalid status")
	}
	if status.Component != c.component ||
		status.MachineID != machineID ||
		!lowerSHA256.MatchString(status.ConfigSHA256) {
		return nodectl.Status{}, errors.New("production controller: status identity mismatch")
	}
	return status, nil
}

func (c *FixedController) Shutdown(ctx context.Context) error {
	if c == nil || c.dialer == nil || c.pipeName == "" {
		return errors.New("production controller: unavailable")
	}
	client := nodectl.NewClient(func(ctx context.Context) (net.Conn, error) {
		return c.dialer.Dial(ctx, c.pipeName)
	})
	if err := client.Shutdown(ctx); err != nil {
		return errors.New("production controller: shutdown unavailable")
	}
	return nil
}

var lowerSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

func validMachineID(value string) bool {
	return machineid.Valid(value)
}

type StatusController interface {
	Status(context.Context) (nodectl.Status, error)
}

type WorkerProvider struct{ controller StatusController }

func NewWorkerProvider(controller StatusController) *WorkerProvider {
	return &WorkerProvider{controller: controller}
}

func (p *WorkerProvider) Snapshot(ctx context.Context) ([]traymodel.WorkerState, error) {
	if p == nil || p.controller == nil {
		return nil, errors.New("production workers: unavailable")
	}
	status, err := p.controller.Status(ctx)
	if err != nil {
		return nil, errors.New("production workers: status unavailable")
	}
	if status.Component != nodectl.ComponentAgent {
		return nil, errors.New("production workers: invalid component")
	}
	workers := make([]traymodel.WorkerState, len(status.Workers))
	for i, worker := range status.Workers {
		workers[i] = traymodel.WorkerState{
			Index: worker.Index, PID: worker.PID, Ready: worker.Ready,
			CurrentTaskSummary: nodectl.SanitizeSummary(worker.CurrentTaskSummary),
			LastErrorSummary:   nodectl.SanitizeSummary(worker.LastErrorSummary),
		}
	}
	return workers, nil
}

type ExplorerBackend interface {
	Start(context.Context, string, []string) error
}

type LocationOpener struct{ backend ExplorerBackend }

func NewLocationOpener(backend ExplorerBackend) *LocationOpener {
	if backend == nil {
		backend = nativeExplorerBackend()
	}
	return &LocationOpener{backend: backend}
}

func (o *LocationOpener) Open(ctx context.Context, path string) error {
	if o == nil || o.backend == nil || !filepath.IsAbs(path) {
		return errors.New("production location opener: invalid location")
	}
	return o.backend.Start(ctx, "explorer.exe", []string{filepath.Clean(path)})
}
