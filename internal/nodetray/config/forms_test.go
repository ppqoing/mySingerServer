package config

import (
	"net/url"
	"reflect"
	"strings"
	"testing"

	agentconfig "dedup/internal/config"
	"dedup/internal/helper"
)

func TestAgentFormRoundTripsEverySupportedFieldWithoutExposingPassword(t *testing.T) {
	cfg := fullyPopulatedAgentConfig()

	form, err := AgentToForm(cfg)
	if err != nil {
		t.Fatalf("AgentToForm: %v", err)
	}
	if form.Database.Password != "" || !form.Database.PasswordStored || form.Database.ReplacePassword {
		t.Fatalf("database secret state = %#v", form.Database)
	}
	if strings.Contains(strings.Join(form.Scan.ImageExts, "|"), "secret") {
		t.Fatal("unexpected secret in non-secret form fields")
	}

	roundTrip, err := AgentFromForm(form, cfg)
	if err != nil {
		t.Fatalf("AgentFromForm: %v", err)
	}
	if !reflect.DeepEqual(roundTrip, cfg) {
		t.Fatalf("round trip mismatch\n got: %#v\nwant: %#v", roundTrip, cfg)
	}

	form.Scan.ImageExts[0] = ".changed"
	if cfg.Scan.ImageExts[0] == ".changed" {
		t.Fatal("AgentToForm shared ImageExts with source config")
	}
	roundTrip.Scan.VideoExts[0] = ".changed"
	if cfg.Scan.VideoExts[0] == ".changed" {
		t.Fatal("AgentFromForm shared VideoExts with base config")
	}
}

func TestAgentFromFormOnlyChangesPasswordWhenExplicitlyRequested(t *testing.T) {
	base := fullyPopulatedAgentConfig()
	form, err := AgentToForm(base)
	if err != nil {
		t.Fatal(err)
	}
	form.Database.User = "new@user"
	form.Database.Password = "ignored-secret"

	preserved, err := AgentFromForm(form, base)
	if err != nil {
		t.Fatal(err)
	}
	preservedURL, err := url.Parse(preserved.PGDSN)
	if err != nil {
		t.Fatal(err)
	}
	password, stored := preservedURL.User.Password()
	if !stored || password != "p@ss:word" || preservedURL.User.Username() != "new@user" {
		t.Fatalf("preserved credentials = %q, %q, %v", preservedURL.User.Username(), password, stored)
	}

	form.Database.ReplacePassword = true
	form.Database.Password = "n@w:/?"
	replaced, err := AgentFromForm(form, base)
	if err != nil {
		t.Fatal(err)
	}
	replacedURL, err := url.Parse(replaced.PGDSN)
	if err != nil {
		t.Fatal(err)
	}
	password, stored = replacedURL.User.Password()
	if !stored || password != "n@w:/?" {
		t.Fatalf("replacement password = %q, stored=%v", password, stored)
	}
	if strings.Contains(replaced.PGDSN, "n@w:/?") {
		t.Fatalf("DSN contains unescaped password: %q", replaced.PGDSN)
	}

	form.Database.Password = ""
	cleared, err := AgentFromForm(form, base)
	if err != nil {
		t.Fatal(err)
	}
	clearedURL, err := url.Parse(cleared.PGDSN)
	if err != nil {
		t.Fatal(err)
	}
	if _, stored := clearedURL.User.Password(); stored {
		t.Fatalf("explicit clear retained a password in %q", cleared.PGDSN)
	}
}

func TestAgentFormBuildsCanonicalEscapedDSNAndReturnsSanitizedFieldError(t *testing.T) {
	base := fullyPopulatedAgentConfig()
	form, err := AgentToForm(base)
	if err != nil {
		t.Fatal(err)
	}
	form.Database = DatabaseForm{
		Host:            "db.example",
		Port:            5433,
		Database:        "media name",
		User:            "user@realm",
		Password:        "secret:/?",
		ReplacePassword: true,
		SSLMode:         "verify-full",
	}

	cfg, err := AgentFromForm(form, base)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PGDSN != "postgres://user%40realm:secret%3A%2F%3F@db.example:5433/media%20name?sslmode=verify-full" {
		t.Fatalf("PGDSN = %q", cfg.PGDSN)
	}

	form.ListenHost = "bad:host:value"
	_, err = AgentFromForm(form, base)
	if err == nil {
		t.Fatal("AgentFromForm accepted malformed listen host")
	}
	fieldErr, ok := err.(*FieldError)
	if !ok || fieldErr.Field != "listenHost" || fieldErr.Code == "" {
		t.Fatalf("error = %#v, want listenHost FieldError", err)
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), base.PGDSN) {
		t.Fatalf("error leaked secret or DSN: %v", err)
	}
}

func TestHelperFormRoundTripsAndDoesNotShareRootSlices(t *testing.T) {
	cfg := helper.Config{
		PipeName:             `\\.\pipe\dedup-delete`,
		AllowedRoots:         []string{`D:\media-a`, `E:\media-b`},
		DeniedRoots:          []string{`D:\media-a\private`},
		DefaultMode:          "soft",
		AllowHardDelete:      false,
		RecycleDirName:       "$Recycle",
		MaxEntriesPerFrame:   99,
		FrameReadTimeoutSec:  33,
		FrameWriteTimeoutSec: 22,
		LogDir:               `D:\logs`,
	}
	form := HelperToForm(cfg)
	roundTrip, err := HelperFromForm(form)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(roundTrip, cfg) {
		t.Fatalf("round trip = %#v, want %#v", roundTrip, cfg)
	}
	form.AllowedRoots[0] = `Z:\changed`
	if cfg.AllowedRoots[0] == `Z:\changed` {
		t.Fatal("HelperToForm shared AllowedRoots with source config")
	}
	roundTrip.DeniedRoots[0] = `Z:\changed`
	if cfg.DeniedRoots[0] == `Z:\changed` {
		t.Fatal("HelperFromForm shared DeniedRoots with source form")
	}
}

func TestAgentToFormDefaultsPostgresPortAndPreservesExplicitPort(t *testing.T) {
	tests := []struct {
		name     string
		dsn      string
		wantHost string
		wantPort int
	}{
		{"DNS default", "postgres://user@db.example/dedup", "db.example", 5432},
		{"IPv4 default", "postgres://user@192.0.2.1/dedup", "192.0.2.1", 5432},
		{"IPv6 default", "postgres://user@[2001:db8::1]/dedup", "2001:db8::1", 5432},
		{"DNS explicit", "postgres://user@db.example:6543/dedup", "db.example", 6543},
		{"IPv6 explicit", "postgres://user@[2001:db8::1]:6544/dedup", "2001:db8::1", 6544},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := fullyPopulatedAgentConfig()
			cfg.PGDSN = tt.dsn
			form, err := AgentToForm(cfg)
			if err != nil {
				t.Fatalf("AgentToForm: %v", err)
			}
			if form.Database.Host != tt.wantHost || form.Database.Port != tt.wantPort {
				t.Fatalf("database endpoint = %q:%d, want %q:%d", form.Database.Host, form.Database.Port, tt.wantHost, tt.wantPort)
			}
		})
	}

	for _, dsn := range []string{
		"postgres://user@/dedup",
		"postgres://user@db.example:0/dedup",
		"postgres://user@db.example:65536/dedup",
		"postgres://user@2001:db8::1/dedup",
	} {
		cfg := fullyPopulatedAgentConfig()
		cfg.PGDSN = dsn
		if _, err := AgentToForm(cfg); err == nil {
			t.Fatalf("AgentToForm accepted invalid endpoint in %q", dsn)
		}
	}
}

func TestAgentFromFormValidatesListenAndDatabaseHostsWithoutRejectingDNSOrIP(t *testing.T) {
	base := fullyPopulatedAgentConfig()
	baseForm, err := AgentToForm(base)
	if err != nil {
		t.Fatal(err)
	}
	valid := []string{"localhost", "db-01.example.com", "db.example.com.", "192.0.2.1", "2001:db8::1"}
	for _, host := range valid {
		t.Run("listen valid/"+host, func(t *testing.T) {
			form := baseForm
			form.ListenHost = host
			if _, err := AgentFromForm(form, base); err != nil {
				t.Fatalf("AgentFromForm rejected valid listen host %q: %v", host, err)
			}
		})
		t.Run("database valid/"+host, func(t *testing.T) {
			form := baseForm
			form.Database.Host = host
			if _, err := AgentFromForm(form, base); err != nil {
				t.Fatalf("AgentFromForm rejected valid database host %q: %v", host, err)
			}
		})
	}

	invalid := []string{
		"bad host", "bad\thost", "secret@host", "host?query", "host#fragment",
		"[::1]", "999.999.999.999", "bad_label", "-bad.example", "bad-.example", "two..labels",
	}
	for _, host := range invalid {
		t.Run("listen invalid/"+host, func(t *testing.T) {
			form := baseForm
			form.ListenHost = host
			_, err := AgentFromForm(form, base)
			assertSanitizedHostFieldError(t, err, "listenHost")
		})
		t.Run("database invalid/"+host, func(t *testing.T) {
			form := baseForm
			form.Database.Host = host
			_, err := AgentFromForm(form, base)
			assertSanitizedHostFieldError(t, err, "database.host")
		})
	}
}

func TestAgentFormAllowsPostgresToRemainUnconfigured(t *testing.T) {
	cfg := fullyPopulatedAgentConfig()
	cfg.PGDSN = ""
	form, err := AgentToForm(cfg)
	if err != nil || form.Database != (DatabaseForm{}) {
		t.Fatalf("AgentToForm local-only = %#v, err=%v", form.Database, err)
	}
	form.ListenHost = "127.0.0.1"
	form.ListenPort = 9101
	converted, err := AgentFromForm(form, cfg)
	if err != nil || converted.PGDSN != "" {
		t.Fatalf("AgentFromForm local-only = %#v, err=%v", converted, err)
	}
}

func TestDatabaseFormAllowsOnlyStableLibpqSSLModeValues(t *testing.T) {
	base := fullyPopulatedAgentConfig()
	baseForm, err := AgentToForm(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"", "disable", "allow", "prefer", "require", "verify-ca", "verify-full"} {
		t.Run("valid/"+mode, func(t *testing.T) {
			form := baseForm
			form.Database.SSLMode = mode
			if _, err := AgentFromForm(form, base); err != nil {
				t.Fatalf("AgentFromForm rejected sslmode %q: %v", mode, err)
			}
		})
	}

	form := baseForm
	form.Database.SSLMode = "require?password=secret"
	_, err = AgentFromForm(form, base)
	assertSanitizedSSLModeFieldError(t, err)

	invalidBase := fullyPopulatedAgentConfig()
	invalidBase.PGDSN = "postgres://user:secret@db.example/dedup?sslmode=secret-mode"
	_, err = AgentToForm(invalidBase)
	assertSanitizedSSLModeFieldError(t, err)
}

func assertSanitizedHostFieldError(t *testing.T, err error, field string) {
	t.Helper()
	fieldErr, ok := err.(*FieldError)
	if !ok || fieldErr.Field != field || fieldErr.Code == "" {
		t.Fatalf("error = %#v, want %s FieldError", err, field)
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "postgres://") {
		t.Fatalf("host error leaked input or DSN: %v", err)
	}
}

func assertSanitizedSSLModeFieldError(t *testing.T, err error) {
	t.Helper()
	fieldErr, ok := err.(*FieldError)
	if !ok || fieldErr.Field != "database.sslMode" || fieldErr.Code == "" {
		t.Fatalf("error = %#v, want database.sslMode FieldError", err)
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "postgres://") {
		t.Fatalf("sslmode error leaked input or DSN: %v", err)
	}
}

func fullyPopulatedAgentConfig() *agentconfig.AgentConfig {
	return &agentconfig.AgentConfig{
		ListenAddr:    "[::1]:9200",
		DataDir:       `D:\data`,
		PGDSN:         "postgres://user:p%40ss%3Aword@db.example:5433/dedup?sslmode=require",
		UseEverything: false,
		Scan: agentconfig.ScanConfig{
			HDDReadBlockMB: 8, HDDStreams: 3, SSDStreams: 7,
			ImageMemResidentMB: 200, ImageTimeoutS: 31, VideoTimeoutS: 121,
			ImageExts: []string{".jpg", ".png"}, VideoExts: []string{".mp4", ".mkv"},
		},
		Sync:     agentconfig.SyncConfig{IntervalS: 301, TriggerRows: 50001, UpsertBatch: 5001},
		Proto:    agentconfig.ProtoConfig{HeartbeatS: 16},
		Worker:   agentconfig.WorkerConfig{Count: 4, ExePath: `D:\bin\worker.exe`, ImageTimeoutS: 32, VideoTimeoutS: 122, ImageMemoryMB: 128, RespawnDelayMS: 501, CrashInjection: true},
		Pipeline: agentconfig.PipelineConfig{ReadChunkKB: 2048},
		Thumb:    agentconfig.ThumbConfig{CacheDir: `D:\cache`, TileMaxSide: 512, ProbeTimeoutS: 17, NativeTimeoutS: 61, FrameTimeoutS: 21},
		IPC:      agentconfig.IPCConfig{MaxFrameMB: 8},
		Delete:   agentconfig.DeleteConfig{PipeName: `\\.\pipe\dedup-delete`, MaxEntriesPerFrame: 1999, DialTimeoutMS: 501, HelloTimeoutS: 6, ReportTimeoutS: 601},
		Tuning:   agentconfig.TuningConfig{StatsEnabled: false, StatsIntervalS: 2, StatsHistoryS: 299, PendingBytesMB: 1023, StatsLogMB: 31, PprofAddr: "127.0.0.1:6060"},
	}
}
