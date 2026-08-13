package traymodel

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStartModeValidateRejectsUnknownValue(t *testing.T) {
	for _, value := range []StartMode{StartManual, StartAutomatic} {
		if err := value.Validate(); err != nil {
			t.Fatalf("Validate(%q): %v", value, err)
		}
	}
	if err := StartMode("sometimes").Validate(); err == nil {
		t.Fatal("Validate accepted unknown start mode")
	}
}

func TestNotificationLevelValidateRejectsUnknownValue(t *testing.T) {
	for _, value := range []NotificationLevel{NotifyImportant, NotifyAll} {
		if err := value.Validate(); err != nil {
			t.Fatalf("Validate(%q): %v", value, err)
		}
	}
	if err := NotificationLevel("none").Validate(); err == nil {
		t.Fatal("Validate accepted unknown notification level")
	}
}

func TestTraySettingsValidateEnforcesVisibleRefreshAndHelperMode(t *testing.T) {
	valid := TraySettings{
		AgentStartMode:         StartManual,
		HelperEnabled:          true,
		HelperStartMode:        StartAutomatic,
		CloseToTray:            false,
		RefreshIntervalSeconds: 2,
		NotificationLevel:      NotifyImportant,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate(valid): %v", err)
	}

	for _, interval := range []int{0, 4} {
		invalid := valid
		invalid.RefreshIntervalSeconds = interval
		if err := invalid.Validate(); err == nil {
			t.Fatalf("Validate accepted refresh interval %d", interval)
		}
	}

	disabledAutomatic := valid
	disabledAutomatic.HelperEnabled = false
	disabledAutomatic.HelperStartMode = StartAutomatic
	if err := disabledAutomatic.Validate(); err == nil {
		t.Fatal("Validate accepted automatic Helper while Helper is disabled")
	}
}

func TestLifecycleRepairModelsExposeStableJSONContract(t *testing.T) {
	force := ForceExitResult{
		OK:               false,
		FailedComponents: []string{"helper", "worker:42"},
		ErrorCode:        "force_exit_failed",
		ErrorSummary:     "后台进程仍在运行",
	}
	config := ConfigApplyResult{
		OK:           true,
		Saved:        true,
		Restarted:    false,
		SHA256:       strings.Repeat("a", 64),
		NeedsRestart: true,
	}
	state := ComponentState{
		RuntimeConfigSHA256: strings.Repeat("b", 64),
		SavedConfigSHA256:   strings.Repeat("a", 64),
		NeedsRestart:        true,
	}

	assertJSONKeys(t, force, "ok", "failedComponents", "errorCode", "errorSummary")
	assertJSONKeys(t, config, "ok", "saved", "restarted", "sha256", "needsRestart", "errorCode", "errorSummary")
	assertJSONKeys(t, state, "runtimeConfigSha256", "savedConfigSha256", "needsRestart")
}

func TestLocalConsoleModelsKeepSecretsOutOfWailsJSON(t *testing.T) {
	preview := LocalDeletePreview{OK: true, BatchID: "batch-1", SelectionDigest: "digest", Count: 2}
	raw, err := json.Marshal(preview)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(raw)), "token") {
		t.Fatalf("delete preview exposed a one-time token: %s", raw)
	}
	assertJSONKeys(t, preview, "ok", "batchId", "selectionDigest", "count", "totalSize", "expiresAt", "files", "errorCode", "errorSummary")

	image := ImagePreview{OK: true, MIME: "image/jpeg", Width: 40, Height: 20, DataBase64: "AA=="}
	assertJSONKeys(t, image, "ok", "mime", "width", "height", "dataBase64", "errorCode", "errorSummary")
}

// Break caught: the Wails directory picker result drops cancellation or a
// stable, display-safe error field before it reaches the local-task UI.
func TestPathSelectionResultExposesWailsSafeJSONContract(t *testing.T) {
	result := PathSelectionResult{
		OK:           true,
		Path:         `D:\Media`,
		Cancelled:    false,
		ErrorCode:    "",
		ErrorSummary: "",
	}
	assertJSONKeys(t, result, "ok", "path", "cancelled", "errorCode", "errorSummary")
}

func assertJSONKeys(t *testing.T, value any, keys ...string) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	for _, key := range keys {
		if _, ok := object[key]; !ok {
			t.Errorf("JSON key %q missing from %s", key, raw)
		}
	}
}
