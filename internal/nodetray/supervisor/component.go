package supervisor

import (
	"errors"
	"time"

	"dedup/internal/nodectl"
	"dedup/internal/nodetray/process"
	"dedup/internal/nodetray/traymodel"
)

func statusClaimError(spec Spec, expected process.Identity, status nodectl.Status) error {
	switch {
	case status.Component != spec.Component:
		return errors.New("control handshake component does not match")
	case status.PID != expected.PID:
		return errors.New("control handshake PID does not match")
	case !process.SamePIDAndExecutable(expected, process.Identity{
		PID: status.PID, ExecutablePath: status.ExecutablePath,
	}):
		return errors.New("control handshake executable path does not match")
	case status.ConfigSHA256 != spec.ExpectedSHA256:
		return errors.New("control handshake config fingerprint does not match")
	default:
		return nil
	}
}

func statusClaimsProcess(spec Spec, expected process.Identity, status nodectl.Status) bool {
	return statusClaimError(spec, expected, status) == nil
}

func statusIsReady(spec Spec, status nodectl.Status) bool {
	if !status.Ready || !status.ServiceReady || status.ConfigSHA256 != spec.ExpectedSHA256 {
		return false
	}
	switch spec.Component {
	case nodectl.ComponentAgent:
		return status.WorkerReady == status.WorkerExpected
	case nodectl.ComponentHelper:
		return status.WorkerExpected == 0 && status.WorkerReady == 0
	default:
		return false
	}
}

func stateFromStatus(spec Spec, identity process.Identity, status nodectl.Status, lifecycle traymodel.Lifecycle) traymodel.ComponentState {
	summary := status.LastErrorSummary
	if summary == "" {
		summary = status.SyncErrorSummary
	}
	ready := statusIsReady(spec, status)
	uptime := int64(0)
	if identity.StartedAtUnixMS > 0 {
		uptime = time.Now().UnixMilli() - identity.StartedAtUnixMS
		if uptime < 0 {
			uptime = 0
		}
		uptime /= 1000
	}
	return traymodel.ComponentState{
		Lifecycle:       lifecycle,
		Healthy:         ready && summary == "",
		Ready:           ready,
		PID:             identity.PID,
		StartedAtUnixMS: identity.StartedAtUnixMS,
		UptimeSeconds:   uptime,
		WorkerReady:     status.WorkerReady,
		WorkerExpected:  status.WorkerExpected,
		ActiveRequests:  status.ActiveRequests,
		ErrorSummary:    nodectl.SanitizeSummary(summary),
	}
}
