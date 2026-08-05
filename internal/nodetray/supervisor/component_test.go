package supervisor

import (
	"strings"
	"testing"

	"dedup/internal/nodectl"
	"dedup/internal/nodetray/process"
	"dedup/internal/nodetray/traymodel"
)

func TestStatusClaimRequiresComponentPIDPathAndConfigFingerprint(t *testing.T) {
	spec := testAgentSpec()
	identity := testIdentity(spec.ExecutablePath, 1001, 123456)
	base := readyAgentStatus(spec, identity)
	cases := []struct {
		name   string
		mutate func(*nodectl.Status)
		want   string
	}{
		{"component", func(s *nodectl.Status) { s.Component = nodectl.ComponentHelper }, "control handshake component does not match"},
		{"pid", func(s *nodectl.Status) { s.PID++ }, "control handshake PID does not match"},
		{"final path", func(s *nodectl.Status) { s.ExecutablePath = `C:\drift\agent.exe` }, "control handshake executable path does not match"},
		{"config fingerprint", func(s *nodectl.Status) { s.ConfigSHA256 = strings.Repeat("b", 64) }, "control handshake config fingerprint does not match"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status := base
			tc.mutate(&status)
			if err := statusClaimError(spec, identity, status); err == nil || err.Error() != tc.want {
				t.Fatalf("statusClaimError = %v, want %q", err, tc.want)
			}
			if statusClaimsProcess(spec, identity, status) {
				t.Fatal("mismatching control handshake was accepted")
			}
		})
	}
	if !statusClaimsProcess(spec, identity, base) {
		t.Fatal("matching control handshake was rejected")
	}

	reportedTimeDrift := base
	reportedTimeDrift.StartedAtUnixMS += 250
	if !statusClaimsProcess(spec, identity, reportedTimeDrift) {
		t.Fatal("matching PID, path and fingerprint was rejected because self-reported time drifted")
	}
	state := stateFromStatus(spec, identity, reportedTimeDrift, traymodel.Running)
	if state.PID != identity.PID || state.StartedAtUnixMS != identity.StartedAtUnixMS {
		t.Fatalf("state used self-reported identity: %#v", state)
	}
}

func TestComponentReadinessUsesComponentSpecificContract(t *testing.T) {
	agentSpec := testAgentSpec()
	identity := testIdentity(agentSpec.ExecutablePath, 1001, 123456)
	agent := readyAgentStatus(agentSpec, identity)
	if !statusIsReady(agentSpec, agent) {
		t.Fatal("fully ready Agent was rejected")
	}
	agent.WorkerReady = agent.WorkerExpected - 1
	if statusIsReady(agentSpec, agent) {
		t.Fatal("Agent with a missing Worker Ready was accepted")
	}

	helperSpec := testHelperSpec()
	helperIdentity := testIdentity(helperSpec.ExecutablePath, 1002, 123457)
	helper := readyHelperStatus(helperSpec, helperIdentity)
	if !statusIsReady(helperSpec, helper) {
		t.Fatal("ready Helper with matching fingerprint was rejected")
	}
	helper.ConfigSHA256 = strings.Repeat("b", 64)
	if statusIsReady(helperSpec, helper) {
		t.Fatal("Helper with a mismatching fingerprint was accepted")
	}
}

func testIdentity(path string, pid int, started int64) process.Identity {
	return process.Identity{PID: pid, StartedAtUnixMS: started, ExecutablePath: path}
}
