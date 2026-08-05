package production

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"dedup/internal/nodectl"
	"dedup/internal/nodetray/bootstrap"
	"dedup/internal/nodetray/process"
	"dedup/internal/nodetray/supervisor"
	"dedup/internal/nodetray/traymodel"
)

type managedController struct {
	mu            sync.Mutex
	statuses      []nodectl.Status
	err           error
	calls         int
	machineID     string
	machineResult traymodel.OperationResult
}

func (c *managedController) Status(context.Context) (nodectl.Status, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.err != nil {
		return nodectl.Status{}, c.err
	}
	if len(c.statuses) == 0 {
		return nodectl.Status{}, errors.New("no status")
	}
	value := c.statuses[0]
	if len(c.statuses) > 1 {
		c.statuses = c.statuses[1:]
	}
	return value, nil
}
func (*managedController) Shutdown(context.Context) error { return nil }
func (c *managedController) UpdateExpectedMachineID(value string) traymodel.OperationResult {
	c.machineID = value
	if c.machineResult.ErrorCode != "" || c.machineResult.OK {
		return c.machineResult
	}
	return traymodel.OperationResult{OK: true}
}

type managedInspector struct {
	mu         sync.Mutex
	identities []process.Identity
	err        error
	calls      int
}

func (i *managedInspector) Inspect(int) (process.Identity, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.calls++
	if i.err != nil {
		return process.Identity{}, i.err
	}
	if len(i.identities) == 0 {
		return process.Identity{}, errors.New("no identity")
	}
	value := i.identities[0]
	if len(i.identities) > 1 {
		i.identities = i.identities[1:]
	}
	return value, nil
}
func (*managedInspector) Wait(ctx context.Context, _ process.Identity) (int, error) {
	<-ctx.Done()
	return 0, ctx.Err()
}

type managedLauncher struct{ calls int }

func (l *managedLauncher) Start(context.Context, string, []string, []string) (process.Identity, error) {
	l.calls++
	return process.Identity{}, errors.New("not started in test")
}

type managedTerminator struct{ calls int }

func (t *managedTerminator) Terminate(process.Identity, uint32) error { t.calls++; return nil }

func managedIdentity() process.Identity {
	return process.Identity{PID: 4101, StartedAtUnixMS: 1_750_000_000_000, ExecutablePath: `C:\Program Files\MySingerServer\agent.exe`}
}

func managedStatus(identity process.Identity) nodectl.Status {
	return nodectl.Status{
		Component: nodectl.ComponentAgent, MachineID: "node-a", PID: identity.PID,
		StartedAtUnixMS: identity.StartedAtUnixMS, ExecutablePath: identity.ExecutablePath,
		ConfigSHA256: strings.Repeat("a", 64), Lifecycle: "running", Ready: true, ServiceReady: true,
	}
}

func newManagedForTest(controller *managedController, inspector *managedInspector) *ManagedComponent {
	identity := managedIdentity()
	s := supervisor.New(supervisor.Spec{
		Component: nodectl.ComponentAgent, ExecutablePath: identity.ExecutablePath,
		ConfigPath: `C:\ProgramData\MySingerServer\Node\agent.json`, ExpectedSHA256: strings.Repeat("a", 64),
		ReadyTimeout: 30 * time.Second, StopTimeout: 15 * time.Second,
	}, &managedLauncher{}, inspector, controller, &managedTerminator{})
	return NewManagedComponent(s, controller, inspector)
}

func TestManagedAdoptRejectsPIDOrPathDriftBeforeSupervisorClaim(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*process.Identity)
	}{
		{name: "pid", mutate: func(value *process.Identity) { value.PID++ }},
		{name: "path", mutate: func(value *process.Identity) { value.ExecutablePath = `C:\private\other.exe` }},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := managedIdentity()
			actual := candidate
			test.mutate(&actual)
			controller := &managedController{statuses: []nodectl.Status{managedStatus(candidate)}}
			inspector := &managedInspector{identities: []process.Identity{actual}}
			managed := newManagedForTest(controller, inspector)

			result := managed.Adopt(context.Background())
			if result.OK || result.ErrorCode != "identity_mismatch" || result.ErrorSummary != "组件身份已变化" {
				t.Fatalf("Adopt drift result = %#v", result)
			}
			if controller.calls != 1 || inspector.calls != 1 {
				t.Fatalf("drift reached Supervisor adoption: status=%d inspect=%d", controller.calls, inspector.calls)
			}
		})
	}
}

func TestManagedAdoptUsesInspectedCreationTimeInsteadOfReportedTime(t *testing.T) {
	identity := managedIdentity()
	reported := managedStatus(identity)
	reported.StartedAtUnixMS += 250
	controller := &managedController{statuses: []nodectl.Status{reported, reported, reported}}
	inspector := &managedInspector{identities: []process.Identity{identity, identity, identity}}
	managed := newManagedForTest(controller, inspector)

	if result := managed.Adopt(context.Background()); !result.OK {
		t.Fatalf("Adopt = %#v", result)
	}
	state := managed.Refresh(context.Background())
	if state.StartedAtUnixMS != identity.StartedAtUnixMS {
		t.Fatalf("Refresh used self-reported identity: %#v", state)
	}
}

func TestManagedAdoptRejectsInspectedIdentityDriftInsideSupervisor(t *testing.T) {
	first := managedIdentity()
	second := first
	second.StartedAtUnixMS++
	status := managedStatus(first)
	controller := &managedController{statuses: []nodectl.Status{status, status}}
	inspector := &managedInspector{identities: []process.Identity{first, second}}
	managed := newManagedForTest(controller, inspector)

	result := managed.Adopt(context.Background())
	if result.OK || result.ErrorCode != "unclaimed_instance" {
		t.Fatalf("Adopt = %#v", result)
	}
}

func TestManagedAdoptRepeatsIdentityAndStatusChecksInsideSupervisor(t *testing.T) {
	identity := managedIdentity()
	secondStatus := managedStatus(identity)
	secondStatus.ExecutablePath = `C:\private\replacement.exe`
	controller := &managedController{statuses: []nodectl.Status{managedStatus(identity), secondStatus}}
	inspector := &managedInspector{identities: []process.Identity{identity, identity}}
	managed := newManagedForTest(controller, inspector)

	result := managed.Adopt(context.Background())
	if result.OK || result.ErrorCode != "unclaimed_instance" || result.ErrorSummary != "组件认领失败" {
		t.Fatalf("Adopt second-check drift = %#v", result)
	}
	if controller.calls != 2 || inspector.calls != 2 {
		t.Fatalf("triple-check calls status=%d inspect=%d", controller.calls, inspector.calls)
	}
}

func TestManagedAdoptSuccessAndOperationsDelegateOneSupervisor(t *testing.T) {
	identity := managedIdentity()
	controller := &managedController{statuses: []nodectl.Status{managedStatus(identity), managedStatus(identity), managedStatus(identity)}}
	inspector := &managedInspector{identities: []process.Identity{identity, identity, identity}}
	managed := newManagedForTest(controller, inspector)
	if result := managed.Adopt(context.Background()); !result.OK {
		t.Fatalf("Adopt = %#v", result)
	}
	state := managed.Refresh(context.Background())
	if state.Lifecycle != traymodel.Running || state.PID != identity.PID {
		t.Fatalf("Refresh = %#v", state)
	}
}

func TestUninitializedSharedComponentForceStopIsIdempotent(t *testing.T) {
	shared := &SharedComponent{}
	if result := shared.ForceStopTracked(context.Background()); !result.OK {
		t.Fatalf("ForceStopTracked = %#v", result)
	}
	if result := shared.Start(context.Background()); result.OK || result.ErrorCode != "unavailable" {
		t.Fatalf("Start = %#v", result)
	}
	if state := shared.Refresh(context.Background()); state.ErrorCode != "unavailable" {
		t.Fatalf("Refresh = %#v", state)
	}
}

type managedFingerprintSource struct {
	agent, helper string
	agentErr      error
	helperErr     error
}

func (s managedFingerprintSource) AgentFingerprint() (string, error)  { return s.agent, s.agentErr }
func (s managedFingerprintSource) HelperFingerprint() (string, error) { return s.helper, s.helperErr }

func TestFactoryFreezesTimeoutsAndSharesOneComponentWithBootstrapAndApp(t *testing.T) {
	identity := managedIdentity()
	controller := &managedController{statuses: []nodectl.Status{managedStatus(identity)}}
	inspector := &managedInspector{identities: []process.Identity{identity}}
	var specs []supervisor.Spec
	factory := NewFactory(FactoryOptions{
		ReadyTimeout: 30 * time.Second,
		StopTimeout:  15 * time.Second,
		Fingerprints: managedFingerprintSource{agent: strings.Repeat("a", 64), helper: strings.Repeat("b", 64)},
		Agent:        ComponentDefinition{Component: nodectl.ComponentAgent, ExecutablePath: identity.ExecutablePath, Launcher: &managedLauncher{}, Inspector: inspector, Controller: func(context.Context) (supervisor.Controller, error) { return controller, nil }, Terminator: &managedTerminator{}},
		Helper:       ComponentDefinition{Component: nodectl.ComponentHelper, ExecutablePath: `C:\Program Files\MySingerServer\helper.exe`, Launcher: &managedLauncher{}, Inspector: inspector, Controller: func(context.Context) (supervisor.Controller, error) { return controller, nil }, Terminator: &managedTerminator{}},
		NewSupervisor: func(spec supervisor.Spec, launcher supervisor.Launcher, inspector process.Inspector, controller supervisor.Controller, terminator supervisor.Terminator) *supervisor.Supervisor {
			specs = append(specs, spec)
			return supervisor.New(spec, launcher, inspector, controller, terminator)
		},
	})
	paths := bootstrap.Paths{AgentConfig: `C:\ProgramData\MySingerServer\Node\agent.json`, HelperConfig: `C:\ProgramData\MySingerServer\Helper\helper.json`}
	fromBootstrap, err := factory.NewAgent(context.Background(), paths)
	if err != nil {
		t.Fatal(err)
	}
	again, err := factory.NewAgent(context.Background(), paths)
	if err != nil {
		t.Fatal(err)
	}
	fromApp := factory.Agent()
	if fromBootstrap != fromApp || again != fromApp {
		t.Fatal("bootstrap and app did not receive the same shared Agent component")
	}
	if len(specs) != 1 {
		t.Fatalf("Supervisor constructions = %d", len(specs))
	}
	want := supervisor.Spec{Component: nodectl.ComponentAgent, ExecutablePath: identity.ExecutablePath, ConfigPath: paths.AgentConfig, ExpectedSHA256: strings.Repeat("a", 64), ReadyTimeout: 30 * time.Second, StopTimeout: 15 * time.Second}
	if !reflect.DeepEqual(specs[0], want) {
		t.Fatalf("Supervisor spec = %#v, want %#v", specs[0], want)
	}
}

func TestFactoryMissingAgentConfigLeavesAgentUnavailableAndHelperBuildable(t *testing.T) {
	identity := managedIdentity()
	controller := &managedController{statuses: []nodectl.Status{managedStatus(identity)}}
	inspector := &managedInspector{identities: []process.Identity{identity}}
	created := 0
	factory := NewFactory(FactoryOptions{
		ReadyTimeout: 30 * time.Second,
		StopTimeout:  15 * time.Second,
		Fingerprints: managedFingerprintSource{agentErr: os.ErrNotExist, helper: strings.Repeat("b", 64)},
		Agent:        ComponentDefinition{Component: nodectl.ComponentAgent, ExecutablePath: identity.ExecutablePath, Launcher: &managedLauncher{}, Inspector: inspector, Controller: func(context.Context) (supervisor.Controller, error) { created++; return controller, nil }, Terminator: &managedTerminator{}},
		Helper:       ComponentDefinition{Component: nodectl.ComponentHelper, ExecutablePath: `C:\Program Files\MySingerServer\helper.exe`, Launcher: &managedLauncher{}, Inspector: inspector, Controller: func(context.Context) (supervisor.Controller, error) { created++; return controller, nil }, Terminator: &managedTerminator{}},
	})
	paths := bootstrap.Paths{AgentConfig: `C:\ProgramData\MySingerServer\Node\agent.json`, HelperConfig: `C:\ProgramData\MySingerServer\Helper\helper.json`}
	component, err := factory.NewAgent(context.Background(), paths)
	if component != factory.Agent() || err == nil {
		t.Fatalf("missing Agent config component=%#v err=%v", component, err)
	}
	if state := component.Refresh(context.Background()); state.ErrorCode != "unavailable" || !state.NeedsAttention {
		t.Fatalf("unavailable Agent Refresh = %#v", state)
	}
	if result := factory.Agent().Start(context.Background()); result.OK || result.ErrorCode != "unavailable" || result.ErrorSummary != "组件不可用" {
		t.Fatalf("unavailable Agent Start = %#v", result)
	}
	if component, err := factory.NewHelper(context.Background(), paths); component == nil || err != nil {
		t.Fatalf("Helper component=%#v err=%v", component, err)
	}
	if created != 1 {
		t.Fatalf("controller factory calls = %d, want only Helper", created)
	}
}

func TestMissingAgentCanBeInitializedInProcessAfterUISavesConfiguration(t *testing.T) {
	identity := managedIdentity()
	firstStatus := managedStatus(identity)
	firstStatus.ConfigSHA256 = strings.Repeat("c", 64)
	secondStatus := firstStatus
	controller := &managedController{statuses: []nodectl.Status{firstStatus, secondStatus}}
	inspector := &managedInspector{identities: []process.Identity{identity, identity}}
	created := 0
	factory := NewFactory(FactoryOptions{
		ReadyTimeout: 30 * time.Second,
		StopTimeout:  15 * time.Second,
		Fingerprints: managedFingerprintSource{agentErr: os.ErrNotExist},
		Agent:        ComponentDefinition{Component: nodectl.ComponentAgent, ExecutablePath: identity.ExecutablePath, Launcher: &managedLauncher{}, Inspector: inspector, Controller: func(context.Context) (supervisor.Controller, error) { created++; return controller, nil }, Terminator: &managedTerminator{}},
	})
	paths := bootstrap.Paths{AgentConfig: `C:\ProgramData\MySingerServer\Node\agent.json`}
	shared, err := factory.NewAgent(context.Background(), paths)
	if shared != factory.Agent() || err == nil {
		t.Fatalf("initial missing Agent component=%#v err=%v", shared, err)
	}
	if result := factory.Agent().UpdateExpectedMachineID("node-new"); !result.OK {
		t.Fatalf("pending MachineID update = %#v", result)
	}
	if result := factory.Agent().UpdateExpectedSHA256(strings.Repeat("c", 64)); !result.OK {
		t.Fatalf("saved fingerprint initialization = %#v", result)
	}
	again, err := factory.NewAgent(context.Background(), paths)
	if err != nil || again != factory.Agent() {
		t.Fatalf("Agent after UI save component=%#v err=%v", again, err)
	}
	if created != 1 || controller.machineID != "node-new" {
		t.Fatalf("controller creations=%d machineID=%q", created, controller.machineID)
	}
	if result := factory.Agent().Adopt(context.Background()); !result.OK {
		t.Fatalf("Adopt after UI save = %#v", result)
	}
}

func TestMissingAgentNeverPublishesComponentWhenPendingIdentityUpdateFails(t *testing.T) {
	identity := managedIdentity()
	controller := &managedController{machineResult: traymodel.OperationResult{ErrorCode: "invalid_config"}}
	createdSupervisors := 0
	factory := NewFactory(FactoryOptions{
		ReadyTimeout: 30 * time.Second,
		StopTimeout:  15 * time.Second,
		Fingerprints: managedFingerprintSource{agentErr: os.ErrNotExist},
		Agent:        ComponentDefinition{Component: nodectl.ComponentAgent, ExecutablePath: identity.ExecutablePath, Launcher: &managedLauncher{}, Inspector: &managedInspector{}, Controller: func(context.Context) (supervisor.Controller, error) { return controller, nil }, Terminator: &managedTerminator{}},
		NewSupervisor: func(spec supervisor.Spec, launcher supervisor.Launcher, inspector process.Inspector, controller supervisor.Controller, terminator supervisor.Terminator) *supervisor.Supervisor {
			createdSupervisors++
			return supervisor.New(spec, launcher, inspector, controller, terminator)
		},
	})
	paths := bootstrap.Paths{AgentConfig: `C:\ProgramData\MySingerServer\Node\agent.json`}
	_, _ = factory.NewAgent(context.Background(), paths)
	if result := factory.Agent().UpdateExpectedMachineID("node-new"); !result.OK {
		t.Fatalf("pending MachineID = %#v", result)
	}
	if result := factory.Agent().UpdateExpectedSHA256(strings.Repeat("c", 64)); result.OK || result.ErrorCode != "unavailable" {
		t.Fatalf("failed identity initialization = %#v", result)
	}
	if createdSupervisors != 0 {
		t.Fatalf("published Supervisor before identity update: %d", createdSupervisors)
	}
	if state := factory.Agent().Refresh(context.Background()); state.ErrorCode != "unavailable" {
		t.Fatalf("failed component became visible: %#v", state)
	}
}

func TestConcurrentIdentityUpdateWaitsForInitializationAndAppliesLatestValue(t *testing.T) {
	identity := managedIdentity()
	controller := &managedController{}
	controllerEntered := make(chan struct{})
	releaseController := make(chan struct{})
	factory := NewFactory(FactoryOptions{
		ReadyTimeout: 30 * time.Second,
		StopTimeout:  15 * time.Second,
		Fingerprints: managedFingerprintSource{agentErr: os.ErrNotExist},
		Agent: ComponentDefinition{Component: nodectl.ComponentAgent, ExecutablePath: identity.ExecutablePath, Launcher: &managedLauncher{}, Inspector: &managedInspector{}, Controller: func(context.Context) (supervisor.Controller, error) {
			close(controllerEntered)
			<-releaseController
			return controller, nil
		}, Terminator: &managedTerminator{}},
	})
	_, _ = factory.NewAgent(context.Background(), bootstrap.Paths{AgentConfig: `C:\ProgramData\MySingerServer\Node\agent.json`})
	if result := factory.Agent().UpdateExpectedMachineID("node-old"); !result.OK {
		t.Fatal(result)
	}
	initialized := make(chan traymodel.OperationResult, 1)
	go func() { initialized <- factory.Agent().UpdateExpectedSHA256(strings.Repeat("c", 64)) }()
	<-controllerEntered
	updated := make(chan traymodel.OperationResult, 1)
	go func() { updated <- factory.Agent().UpdateExpectedMachineID("node-new") }()
	select {
	case result := <-updated:
		t.Fatalf("concurrent identity update returned before initialization publish: %#v", result)
	case <-time.After(25 * time.Millisecond):
	}
	close(releaseController)
	if result := <-initialized; !result.OK {
		t.Fatalf("initialization = %#v", result)
	}
	if result := <-updated; !result.OK {
		t.Fatalf("latest identity update = %#v", result)
	}
	if controller.machineID != "node-new" {
		t.Fatalf("published controller machineID = %q", controller.machineID)
	}
}

func TestConcurrentFirstFingerprintUpdatesApplyLatestSHAAfterInitialization(t *testing.T) {
	identity := managedIdentity()
	latestStatus := managedStatus(identity)
	latestStatus.ConfigSHA256 = strings.Repeat("d", 64)
	controller := &managedController{statuses: []nodectl.Status{latestStatus, latestStatus}}
	inspector := &managedInspector{identities: []process.Identity{identity, identity}}
	controllerEntered := make(chan struct{})
	releaseController := make(chan struct{})
	factory := NewFactory(FactoryOptions{
		ReadyTimeout: 30 * time.Second,
		StopTimeout:  15 * time.Second,
		Fingerprints: managedFingerprintSource{agentErr: os.ErrNotExist},
		Agent: ComponentDefinition{Component: nodectl.ComponentAgent, ExecutablePath: identity.ExecutablePath, Launcher: &managedLauncher{}, Inspector: inspector, Controller: func(context.Context) (supervisor.Controller, error) {
			close(controllerEntered)
			<-releaseController
			return controller, nil
		}, Terminator: &managedTerminator{}},
	})
	_, _ = factory.NewAgent(context.Background(), bootstrap.Paths{AgentConfig: `C:\ProgramData\MySingerServer\Node\agent.json`})
	first := make(chan traymodel.OperationResult, 1)
	go func() { first <- factory.Agent().UpdateExpectedSHA256(strings.Repeat("c", 64)) }()
	<-controllerEntered
	second := make(chan traymodel.OperationResult, 1)
	go func() { second <- factory.Agent().UpdateExpectedSHA256(strings.Repeat("d", 64)) }()
	select {
	case result := <-second:
		t.Fatalf("latest fingerprint returned before first initialization completed: %#v", result)
	case <-time.After(25 * time.Millisecond):
	}
	close(releaseController)
	if result := <-first; !result.OK {
		t.Fatalf("first fingerprint update = %#v", result)
	}
	if result := <-second; !result.OK {
		t.Fatalf("latest fingerprint update = %#v", result)
	}
	if result := factory.Agent().Adopt(context.Background()); !result.OK {
		t.Fatalf("latest fingerprint was not applied before Adopt: %#v", result)
	}
}
