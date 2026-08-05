//go:build windows

package task

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefinitionBuildsOnlyTheFixedHighestInteractiveLogonTask(t *testing.T) {
	root := t.TempDir()
	helper := filepath.Join(root, "bin", "helper.exe")
	config := filepath.Join(root, "config", "helper.json")
	wantSID := "S-1-5-21-100-200-300-1001"

	registration, err := buildTaskRegistration(Definition{
		HelperExecutable: helper,
		HelperConfig:     config,
		UserSID:          wantSID,
	}, func(value string) (string, error) {
		return filepath.Clean(value), nil
	})
	if err != nil {
		t.Fatalf("buildTaskRegistration: %v", err)
	}

	if registration.Path != TaskPath {
		t.Fatalf("Path = %q, want %q", registration.Path, TaskPath)
	}
	if registration.Principal.UserSID != wantSID ||
		registration.Principal.LogonType != logonTypeInteractiveToken ||
		registration.Principal.RunLevel != runLevelHighest {
		t.Fatalf("Principal = %#v, want fixed SID/InteractiveToken/Highest", registration.Principal)
	}
	if len(registration.Triggers) != 1 || registration.Triggers[0].Kind != triggerKindLogon ||
		registration.Triggers[0].UserSID != wantSID {
		t.Fatalf("Triggers = %#v, want one LogonTrigger", registration.Triggers)
	}
	if len(registration.Actions) != 1 {
		t.Fatalf("Actions = %#v, want one Exec action", registration.Actions)
	}
	action := registration.Actions[0]
	if action.Kind != actionKindExec || action.Executable != helper ||
		action.Arguments != `--config "`+config+`"` {
		t.Fatalf("Action = %#v, want fixed helper/config Exec", action)
	}
	if registration.MultipleInstancesPolicy != multipleInstancesIgnoreNew {
		t.Fatalf("MultipleInstancesPolicy = %q, want %q", registration.MultipleInstancesPolicy, multipleInstancesIgnoreNew)
	}

	xmlText, err := renderTaskXML(registration)
	if err != nil {
		t.Fatalf("renderTaskXML: %v", err)
	}
	for _, forbidden := range []string{
		"<Password>", "<WorkingDirectory>", "<Environment>", "<CommandLine>",
	} {
		if strings.Contains(xmlText, forbidden) {
			t.Fatalf("task XML contains forbidden element %q", forbidden)
		}
	}
	if strings.Count(xmlText, "<Principal ") != 1 ||
		strings.Count(xmlText, "<LogonTrigger>") != 1 ||
		strings.Count(xmlText, "<Exec>") != 1 {
		t.Fatalf("task XML did not preserve one principal/trigger/action: %s", xmlText)
	}
}

func TestDefinitionEmitsLogonTriggerBaseFieldsBeforeUserID(t *testing.T) {
	root := t.TempDir()
	wantSID := "S-1-5-21-100-200-300-1001"
	registration, err := buildTaskRegistration(Definition{
		HelperExecutable: filepath.Join(root, "helper.exe"),
		HelperConfig:     filepath.Join(root, "helper.json"),
		UserSID:          wantSID,
	}, identityResolver)
	if err != nil {
		t.Fatalf("buildTaskRegistration: %v", err)
	}
	xmlText, err := renderTaskXML(registration)
	if err != nil {
		t.Fatalf("renderTaskXML: %v", err)
	}
	want := "<Triggers><LogonTrigger><Enabled>true</Enabled><UserId>" + wantSID + "</UserId></LogonTrigger></Triggers>"
	if !strings.Contains(xmlText, want) {
		t.Fatalf("Triggers XML does not follow triggerBaseType order; want exact fragment %q in %s", want, xmlText)
	}
}

func TestDefinitionRejectsNonCanonicalOrMalformedSID(t *testing.T) {
	root := t.TempDir()
	helper := filepath.Join(root, "helper.exe")
	config := filepath.Join(root, "helper.json")
	invalid := []string{
		"", "s-1-5-21-1", "S-2-5-21-1", "S-01-5-21-1", "S-1-05-21-1", "S-1-5-021-1",
		"S-1-5", "S-1-5-4294967296", "S-1-281474976710656-1",
		"S-1-5-21-1 ", "S-1-5-21-1\n", "not-a-sid",
	}
	for _, candidate := range invalid {
		t.Run(candidate, func(t *testing.T) {
			_, err := buildTaskRegistration(Definition{
				HelperExecutable: helper,
				HelperConfig:     config,
				UserSID:          candidate,
			}, identityResolver)
			if err == nil {
				t.Fatalf("accepted invalid SID %q", candidate)
			}
		})
	}
}

func TestDefinitionRejectsExecutableDriftAndInjection(t *testing.T) {
	root := t.TempDir()
	helper := filepath.Join(root, "helper.exe")
	config := filepath.Join(root, "helper.json")
	sid := "S-1-5-21-100-200-300-1001"
	invalid := []string{
		"helper.exe",
		filepath.Join(root, "worker.exe"),
		helper + " --evil",
		helper + `" --evil`,
		helper + "\n--evil",
		" " + helper,
	}
	for _, candidate := range invalid {
		t.Run(candidate, func(t *testing.T) {
			_, err := buildTaskRegistration(Definition{
				HelperExecutable: candidate,
				HelperConfig:     config,
				UserSID:          sid,
			}, identityResolver)
			if err == nil {
				t.Fatalf("accepted invalid executable %q", candidate)
			}
		})
	}

	for name, resolved := range map[string]string{
		"other executable": filepath.Join(root, "payload.exe"),
		"relative final":   "helper.exe",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := buildTaskRegistration(Definition{
				HelperExecutable: helper,
				HelperConfig:     config,
				UserSID:          sid,
			}, func(string) (string, error) { return resolved, nil })
			if err == nil {
				t.Fatalf("accepted final executable %q", resolved)
			}
		})
	}

	resolverErr := errors.New("resolver failed")
	_, err := buildTaskRegistration(Definition{
		HelperExecutable: helper,
		HelperConfig:     config,
		UserSID:          sid,
	}, func(string) (string, error) { return "", resolverErr })
	if err == nil || errors.Is(err, resolverErr) {
		t.Fatalf("resolver failure = %v, want stable redacted validation error", err)
	}
}

func TestDefinitionRejectsConfigPathInjection(t *testing.T) {
	root := t.TempDir()
	helper := filepath.Join(root, "helper.exe")
	sid := "S-1-5-21-100-200-300-1001"
	validConfig := filepath.Join(root, "helper.json")
	invalid := []string{
		"helper.json",
		validConfig + `" --evil`,
		validConfig + "\n--evil",
		validConfig + "\r",
		" " + validConfig,
		filepath.Join(root, "config") + string(filepath.Separator),
	}
	for _, candidate := range invalid {
		t.Run(candidate, func(t *testing.T) {
			_, err := buildTaskRegistration(Definition{
				HelperExecutable: helper,
				HelperConfig:     candidate,
				UserSID:          sid,
			}, identityResolver)
			if err == nil {
				t.Fatalf("accepted invalid config %q", candidate)
			}
		})
	}
}

func TestDefinitionRejectsExistingDirectoryAsConfig(t *testing.T) {
	root := t.TempDir()
	helper := filepath.Join(root, "helper.exe")
	configDirectory := filepath.Join(root, "helper.json")
	if err := os.Mkdir(configDirectory, 0o700); err != nil {
		t.Fatalf("create config directory: %v", err)
	}
	_, err := buildTaskRegistration(Definition{
		HelperExecutable: helper,
		HelperConfig:     configDirectory,
		UserSID:          "S-1-5-21-100-200-300-1001",
	}, identityResolver)
	if err == nil {
		t.Fatal("accepted existing directory as Helper config")
	}
}

func identityResolver(value string) (string, error) {
	return filepath.Clean(value), nil
}
