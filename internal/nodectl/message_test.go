package nodectl

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func validAgentStatus() Status {
	return Status{
		Component:       ComponentAgent,
		MachineID:       "node-a",
		PID:             42,
		StartedAtUnixMS: 1,
		ExecutablePath:  `C:\\Program Files\\MySingerServer\\agent.exe`,
		ConfigSHA256:    strings.Repeat("a", 64),
		Lifecycle:       "running",
		ServiceReady:    true,
		Ready:           true,
		WorkerExpected:  2,
		WorkerReady:     2,
		Workers: []WorkerStatus{
			{Index: 0, PID: 100, Ready: true, CurrentTaskSummary: "scan:01"},
			{Index: 1, PID: 101, Ready: true, CurrentTaskSummary: "scan:02"},
		},
		SyncHealthy:    true,
		ActiveRequests: 0,
	}
}

func TestRequestValidateRejectsInvalidProtocolFields(t *testing.T) {
	// This catches accepting requests that the control service must reject before dispatch.
	tests := []struct {
		name string
		in   Request
	}{
		{"wrong version", Request{Version: 2, RequestID: "request-1", Command: CommandStatus}},
		{"empty id", Request{Version: ProtocolVersion, RequestID: "", Command: CommandStatus}},
		{"id over 64 runes", Request{Version: ProtocolVersion, RequestID: strings.Repeat("a", 65), Command: CommandStatus}},
		{"unknown command", Request{Version: ProtocolVersion, RequestID: "request-1", Command: "reboot"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.in.Validate(); err == nil {
				t.Fatal("Validate() error = nil, want rejection")
			}
		})
	}
}

func TestRequestValidateAcceptsStatusAndShutdown(t *testing.T) {
	for _, command := range []Command{CommandStatus, CommandShutdown} {
		request := Request{Version: ProtocolVersion, RequestID: "request-1", Command: command}
		if err := request.Validate(); err != nil {
			t.Fatalf("Validate(%q) error = %v, want nil", command, err)
		}
	}
}

func TestStatusValidateRejectsBoundariesAndInconsistentWorkers(t *testing.T) {
	tooMany := make([]WorkerStatus, 1025)
	for i := range tooMany {
		tooMany[i] = WorkerStatus{Index: i, PID: i + 1}
	}
	tests := []struct {
		name string
		mut  func(*Status)
	}{
		{"path over 1024 bytes", func(s *Status) { s.ExecutablePath = strings.Repeat("x", 1025) }},
		{"error summary over 512 runes", func(s *Status) { s.LastErrorSummary = strings.Repeat("界", 513) }},
		{"negative pid", func(s *Status) { s.PID = -1 }},
		{"negative expected workers", func(s *Status) { s.WorkerExpected = -1 }},
		{"negative active requests", func(s *Status) { s.ActiveRequests = -1 }},
		{"more than 1024 workers", func(s *Status) { s.WorkerExpected, s.WorkerReady, s.Workers = 1025, 0, tooMany }},
		{"duplicate worker index", func(s *Status) { s.Workers[1].Index = 0 }},
		{"worker count mismatch", func(s *Status) { s.Workers = s.Workers[:1] }},
		{"ready count mismatch", func(s *Status) { s.WorkerReady = 1 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := validAgentStatus()
			tt.mut(&status)
			if err := status.Validate(); err == nil {
				t.Fatal("Validate() error = nil, want rejection")
			}
		})
	}
}

func TestStatusValidateAcceptsAgentAndHelper(t *testing.T) {
	if err := validAgentStatus().Validate(); err != nil {
		t.Fatalf("agent Validate() error = %v, want nil", err)
	}
	helper := Status{
		Component:       ComponentHelper,
		MachineID:       "node-a",
		PID:             77,
		StartedAtUnixMS: 1,
		ExecutablePath:  `C:\\Program Files\\MySingerServer\\helper.exe`,
		Lifecycle:       "running",
		ServiceReady:    true,
		Ready:           true,
	}
	if err := helper.Validate(); err != nil {
		t.Fatalf("helper Validate() error = %v, want nil", err)
	}
}

func TestValidateControlIdentityRejectsProtocolBoundariesWithoutEchoingInput(t *testing.T) {
	validPath := `C:\Program Files\MySingerServer\agent.exe`
	if err := ValidateControlIdentity("node-a", validPath); err != nil {
		t.Fatalf("valid identity rejected: %v", err)
	}
	oversizedMachineID := strings.Repeat("界", 129)
	oversizedExecutable := strings.Repeat("x", 1025)
	for _, testCase := range []struct {
		name      string
		machineID string
		exe       string
		secret    string
	}{
		{name: "machine id", machineID: oversizedMachineID, exe: validPath, secret: oversizedMachineID},
		{name: "executable path", machineID: "node-a", exe: oversizedExecutable, secret: oversizedExecutable},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := ValidateControlIdentity(testCase.machineID, testCase.exe)
			if err == nil {
				t.Fatal("ValidateControlIdentity error = nil, want boundary rejection")
			}
			if strings.Contains(err.Error(), testCase.secret) {
				t.Fatalf("identity validation echoed rejected input: %v", err)
			}
		})
	}
}

func TestResponseValidateEnforcesSuccessAndFailureShapes(t *testing.T) {
	good := Response{Version: ProtocolVersion, RequestID: "request-1", OK: true, Status: ptrStatus(validAgentStatus())}
	if err := good.Validate(); err != nil {
		t.Fatalf("success Validate() error = %v, want nil", err)
	}
	for _, response := range []Response{
		{Version: ProtocolVersion, RequestID: "request-1", OK: true, ErrorCode: "internal_error", Status: ptrStatus(validAgentStatus())},
		{Version: ProtocolVersion, RequestID: "request-1", OK: false, ErrorCode: "internal_error", ErrorSummary: "failed", Status: ptrStatus(validAgentStatus())},
		{Version: ProtocolVersion, RequestID: "request-1", OK: false, ErrorCode: "postgres://user:secret@example.invalid/db", ErrorSummary: "failed"},
		{Version: ProtocolVersion, RequestID: "request-1", OK: false, ErrorCode: "unexpected_code", ErrorSummary: "failed"},
	} {
		if err := response.Validate(); err == nil {
			t.Fatal("Validate() error = nil, want invalid response rejected")
		}
	}
}

func TestSanitizeSummaryRemovesControlsRedactsURIAndTruncatesRunes(t *testing.T) {
	input := "before \r\npostgres://user:secret@example.invalid/db\x00 after"
	got := SanitizeSummary(input)
	if got != "before [REDACTED_URI] after" {
		t.Fatalf("SanitizeSummary() = %q, want %q", got, "before [REDACTED_URI] after")
	}
	long := strings.Repeat("界", 513)
	got = SanitizeSummary(long)
	if !utf8.ValidString(got) || utf8.RuneCountInString(got) != 512 {
		t.Fatalf("SanitizeSummary(long) rune count = %d valid=%v, want 512 valid UTF-8", utf8.RuneCountInString(got), utf8.ValidString(got))
	}
}

func TestSanitizeSummaryRedactsURIsAndSecretsInKeyValueContexts(t *testing.T) {
	// This catches diagnostics leaking credentials when a URI or secret follows a key or JSON field.
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"URI after equals", "cause=postgres://user:secret@example.invalid/db", "cause=[REDACTED_URI]"},
		{"JSON password", `{"password":"secret"}`, `{"password":"[REDACTED]"}`},
		{"colon database URL", "database_url: postgres://user:secret@example.invalid/db", "database_url: [REDACTED_URI]"},
		{"colon quoted value with spaces", `password: "first second"`, `password: "[REDACTED]"`},
		{"JSON escaped quotes", `{"password":"first \"second\" tail"}`, `{"password":"[REDACTED]"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SanitizeSummary(tt.in); got != tt.want {
				t.Fatalf("SanitizeSummary() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSanitizeSummaryRedactsUNCMediaPathAndValidationRejectsRawUNC(t *testing.T) {
	unc := `\\fictional-server\fictional-share\private clip.mp4`
	got := SanitizeSummary("decode failed: " + unc)
	if got != "decode failed: [REDACTED_PATH]" {
		t.Fatalf("SanitizeSummary(UNC) = %q", got)
	}
	status := validAgentStatus()
	status.LastErrorSummary = "decode failed: " + unc
	if err := status.Validate(); err == nil {
		t.Fatal("Status.Validate accepted an unsanitized UNC media path")
	}
}

func ptrStatus(value Status) *Status { return &value }
