package config

import (
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode"

	agentconfig "dedup/internal/config"
	"dedup/internal/helper"
)

type AgentForm struct {
	ListenHost    string          `json:"listenHost"`
	ListenPort    int             `json:"listenPort"`
	DataDir       string          `json:"dataDir"`
	Database      DatabaseForm    `json:"database"`
	UseEverything bool            `json:"useEverything"`
	Scan          ScanForm        `json:"scan"`
	Sync          SyncForm        `json:"sync"`
	Proto         ProtoForm       `json:"proto"`
	Worker        WorkerForm      `json:"worker"`
	Pipeline      PipelineForm    `json:"pipeline"`
	Thumb         ThumbForm       `json:"thumb"`
	IPC           IPCForm         `json:"ipc"`
	Delete        AgentDeleteForm `json:"delete"`
	Tuning        TuningForm      `json:"tuning"`
}

type DatabaseForm struct {
	Host            string `json:"host"`
	Port            int    `json:"port"`
	Database        string `json:"database"`
	User            string `json:"user"`
	Password        string `json:"password"`
	PasswordStored  bool   `json:"passwordStored"`
	ReplacePassword bool   `json:"replacePassword"`
	SSLMode         string `json:"sslMode"`
}

type ScanForm struct {
	HDDReadBlockMB     int      `json:"hddReadBlockMb"`
	HDDStreams         int      `json:"hddStreamsPerDisk"`
	SSDStreams         int      `json:"ssdStreamsPerDisk"`
	ImageMemResidentMB int      `json:"imageMemResidentMb"`
	ImageTimeoutS      int      `json:"imageTimeoutS"`
	VideoTimeoutS      int      `json:"videoTimeoutS"`
	ImageExts          []string `json:"imageExts"`
	VideoExts          []string `json:"videoExts"`
}

type SyncForm struct {
	IntervalS   int `json:"intervalS"`
	TriggerRows int `json:"triggerRows"`
	UpsertBatch int `json:"upsertBatch"`
}

type ProtoForm struct {
	HeartbeatS int `json:"heartbeatS"`
}

type WorkerForm struct {
	Count          int    `json:"count"`
	ExePath        string `json:"exePath"`
	ImageTimeoutS  int    `json:"imageTimeoutS"`
	VideoTimeoutS  int    `json:"videoTimeoutS"`
	ImageMemoryMB  int    `json:"imageMemoryMb"`
	RespawnDelayMS int    `json:"respawnDelayMs"`
}

type PipelineForm struct {
	ReadChunkKB int `json:"readChunkKb"`
}

type ThumbForm struct {
	CacheDir       string `json:"cacheDir"`
	TileMaxSide    int    `json:"tileMaxSide"`
	ProbeTimeoutS  int    `json:"probeTimeoutS"`
	NativeTimeoutS int    `json:"nativeTimeoutS"`
	FrameTimeoutS  int    `json:"frameTimeoutS"`
}

type IPCForm struct {
	MaxFrameMB int `json:"maxFrameMb"`
}

type AgentDeleteForm struct {
	PipeName           string `json:"pipeName"`
	MaxEntriesPerFrame int    `json:"maxEntriesPerFrame"`
	DialTimeoutMS      int    `json:"dialTimeoutMs"`
	HelloTimeoutS      int    `json:"helloTimeoutS"`
	ReportTimeoutS     int    `json:"reportTimeoutS"`
}

type TuningForm struct {
	StatsEnabled   bool   `json:"statsEnabled"`
	StatsIntervalS int    `json:"statsIntervalS"`
	StatsHistoryS  int    `json:"statsHistoryS"`
	PendingBytesMB int    `json:"pendingBytesMb"`
	StatsLogMB     int    `json:"statsLogMb"`
	PprofAddr      string `json:"pprofAddr"`
}

type HelperForm struct {
	PipeName             string   `json:"pipeName"`
	AllowedRoots         []string `json:"allowedRoots"`
	DeniedRoots          []string `json:"deniedRoots"`
	DefaultMode          string   `json:"defaultMode"`
	AllowHardDelete      bool     `json:"allowHardDelete"`
	RecycleDirName       string   `json:"recycleDirName"`
	MaxEntriesPerFrame   int      `json:"maxEntriesPerFrame"`
	FrameReadTimeoutSec  int      `json:"frameReadTimeoutSec"`
	FrameWriteTimeoutSec int      `json:"frameWriteTimeoutSec"`
	LogDir               string   `json:"logDir"`
}

type FieldError struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *FieldError) Error() string {
	if e == nil {
		return ""
	}
	return e.Field + ": " + e.Message
}

func AgentToForm(cfg *agentconfig.AgentConfig) (AgentForm, error) {
	if cfg == nil {
		return AgentForm{}, fieldError("agent", "required", "Agent 配置不能为空")
	}
	listenHost, listenPort, err := splitHostPort(cfg.ListenAddr, "listenHost", "listenPort")
	if err != nil {
		return AgentForm{}, err
	}
	database, err := databaseToForm(cfg.PGDSN)
	if err != nil {
		return AgentForm{}, err
	}
	return AgentForm{
		ListenHost:    listenHost,
		ListenPort:    listenPort,
		DataDir:       cfg.DataDir,
		Database:      database,
		UseEverything: cfg.UseEverything,
		Scan: ScanForm{
			HDDReadBlockMB: cfg.Scan.HDDReadBlockMB, HDDStreams: cfg.Scan.HDDStreams,
			SSDStreams: cfg.Scan.SSDStreams, ImageMemResidentMB: cfg.Scan.ImageMemResidentMB,
			ImageTimeoutS: cfg.Scan.ImageTimeoutS, VideoTimeoutS: cfg.Scan.VideoTimeoutS,
			ImageExts: append([]string{}, cfg.Scan.ImageExts...),
			VideoExts: append([]string{}, cfg.Scan.VideoExts...),
		},
		Sync:     SyncForm{IntervalS: cfg.Sync.IntervalS, TriggerRows: cfg.Sync.TriggerRows, UpsertBatch: cfg.Sync.UpsertBatch},
		Proto:    ProtoForm{HeartbeatS: cfg.Proto.HeartbeatS},
		Worker:   WorkerForm{Count: cfg.Worker.Count, ExePath: cfg.Worker.ExePath, ImageTimeoutS: cfg.Worker.ImageTimeoutS, VideoTimeoutS: cfg.Worker.VideoTimeoutS, ImageMemoryMB: cfg.Worker.ImageMemoryMB, RespawnDelayMS: cfg.Worker.RespawnDelayMS},
		Pipeline: PipelineForm{ReadChunkKB: cfg.Pipeline.ReadChunkKB},
		Thumb:    ThumbForm{CacheDir: cfg.Thumb.CacheDir, TileMaxSide: cfg.Thumb.TileMaxSide, ProbeTimeoutS: cfg.Thumb.ProbeTimeoutS, NativeTimeoutS: cfg.Thumb.NativeTimeoutS, FrameTimeoutS: cfg.Thumb.FrameTimeoutS},
		IPC:      IPCForm{MaxFrameMB: cfg.IPC.MaxFrameMB},
		Delete:   AgentDeleteForm{PipeName: cfg.Delete.PipeName, MaxEntriesPerFrame: cfg.Delete.MaxEntriesPerFrame, DialTimeoutMS: cfg.Delete.DialTimeoutMS, HelloTimeoutS: cfg.Delete.HelloTimeoutS, ReportTimeoutS: cfg.Delete.ReportTimeoutS},
		Tuning:   TuningForm{StatsEnabled: cfg.Tuning.StatsEnabled, StatsIntervalS: cfg.Tuning.StatsIntervalS, StatsHistoryS: cfg.Tuning.StatsHistoryS, PendingBytesMB: cfg.Tuning.PendingBytesMB, StatsLogMB: cfg.Tuning.StatsLogMB, PprofAddr: cfg.Tuning.PprofAddr},
	}, nil
}

func AgentFromForm(form AgentForm, base *agentconfig.AgentConfig) (*agentconfig.AgentConfig, error) {
	if err := validateHost(form.ListenHost, "listenHost"); err != nil {
		return nil, err
	}
	if form.ListenPort < 1 || form.ListenPort > 65535 {
		return nil, fieldError("listenPort", "out_of_range", "监听端口必须为 1..65535")
	}
	dsn, err := databaseFromForm(form.Database, base)
	if err != nil {
		return nil, err
	}
	crashInjection := false
	if base != nil {
		crashInjection = base.Worker.CrashInjection
	}
	return &agentconfig.AgentConfig{
		ListenAddr:    net.JoinHostPort(form.ListenHost, strconv.Itoa(form.ListenPort)),
		DataDir:       form.DataDir,
		PGDSN:         dsn,
		UseEverything: form.UseEverything,
		Scan: agentconfig.ScanConfig{
			HDDReadBlockMB: form.Scan.HDDReadBlockMB, HDDStreams: form.Scan.HDDStreams,
			SSDStreams: form.Scan.SSDStreams, ImageMemResidentMB: form.Scan.ImageMemResidentMB,
			ImageTimeoutS: form.Scan.ImageTimeoutS, VideoTimeoutS: form.Scan.VideoTimeoutS,
			ImageExts: append([]string(nil), form.Scan.ImageExts...),
			VideoExts: append([]string(nil), form.Scan.VideoExts...),
		},
		Sync:     agentconfig.SyncConfig{IntervalS: form.Sync.IntervalS, TriggerRows: form.Sync.TriggerRows, UpsertBatch: form.Sync.UpsertBatch},
		Proto:    agentconfig.ProtoConfig{HeartbeatS: form.Proto.HeartbeatS},
		Worker:   agentconfig.WorkerConfig{Count: form.Worker.Count, ExePath: form.Worker.ExePath, ImageTimeoutS: form.Worker.ImageTimeoutS, VideoTimeoutS: form.Worker.VideoTimeoutS, ImageMemoryMB: form.Worker.ImageMemoryMB, RespawnDelayMS: form.Worker.RespawnDelayMS, CrashInjection: crashInjection},
		Pipeline: agentconfig.PipelineConfig{ReadChunkKB: form.Pipeline.ReadChunkKB},
		Thumb:    agentconfig.ThumbConfig{CacheDir: form.Thumb.CacheDir, TileMaxSide: form.Thumb.TileMaxSide, ProbeTimeoutS: form.Thumb.ProbeTimeoutS, NativeTimeoutS: form.Thumb.NativeTimeoutS, FrameTimeoutS: form.Thumb.FrameTimeoutS},
		IPC:      agentconfig.IPCConfig{MaxFrameMB: form.IPC.MaxFrameMB},
		Delete:   agentconfig.DeleteConfig{PipeName: form.Delete.PipeName, MaxEntriesPerFrame: form.Delete.MaxEntriesPerFrame, DialTimeoutMS: form.Delete.DialTimeoutMS, HelloTimeoutS: form.Delete.HelloTimeoutS, ReportTimeoutS: form.Delete.ReportTimeoutS},
		Tuning:   agentconfig.TuningConfig{StatsEnabled: form.Tuning.StatsEnabled, StatsIntervalS: form.Tuning.StatsIntervalS, StatsHistoryS: form.Tuning.StatsHistoryS, PendingBytesMB: form.Tuning.PendingBytesMB, StatsLogMB: form.Tuning.StatsLogMB, PprofAddr: form.Tuning.PprofAddr},
	}, nil
}

func HelperToForm(cfg helper.Config) HelperForm {
	return HelperForm{
		PipeName: cfg.PipeName, AllowedRoots: append([]string{}, cfg.AllowedRoots...),
		DeniedRoots: append([]string{}, cfg.DeniedRoots...), DefaultMode: cfg.DefaultMode,
		AllowHardDelete: cfg.AllowHardDelete, RecycleDirName: cfg.RecycleDirName,
		MaxEntriesPerFrame: cfg.MaxEntriesPerFrame, FrameReadTimeoutSec: cfg.FrameReadTimeoutSec,
		FrameWriteTimeoutSec: cfg.FrameWriteTimeoutSec, LogDir: cfg.LogDir,
	}
}

func HelperFromForm(form HelperForm) (helper.Config, error) {
	return helper.Config{
		PipeName: form.PipeName, AllowedRoots: append([]string(nil), form.AllowedRoots...),
		DeniedRoots: append([]string(nil), form.DeniedRoots...), DefaultMode: form.DefaultMode,
		AllowHardDelete: form.AllowHardDelete, RecycleDirName: form.RecycleDirName,
		MaxEntriesPerFrame: form.MaxEntriesPerFrame, FrameReadTimeoutSec: form.FrameReadTimeoutSec,
		FrameWriteTimeoutSec: form.FrameWriteTimeoutSec, LogDir: form.LogDir,
	}, nil
}

func databaseToForm(dsn string) (DatabaseForm, error) {
	u, err := parsePostgresURL(dsn)
	if err != nil {
		return DatabaseForm{}, err
	}
	host := u.Hostname()
	if err := validateHost(host, "database.host"); err != nil {
		return DatabaseForm{}, err
	}
	port := 5432
	if portText := u.Port(); portText != "" {
		port, err = strconv.Atoi(portText)
		if err != nil || port < 1 || port > 65535 {
			return DatabaseForm{}, fieldError("database.port", "out_of_range", "数据库端口必须为 1..65535")
		}
	}
	database := strings.TrimPrefix(u.Path, "/")
	if database == "" {
		return DatabaseForm{}, fieldError("database.database", "required", "数据库名称不能为空")
	}
	user := ""
	passwordStored := false
	if u.User != nil {
		user = u.User.Username()
		_, passwordStored = u.User.Password()
	}
	sslMode := u.Query().Get("sslmode")
	if err := validateSSLMode(sslMode); err != nil {
		return DatabaseForm{}, err
	}
	return DatabaseForm{
		Host: host, Port: port, Database: database, User: user,
		Password: "", PasswordStored: passwordStored, ReplacePassword: false,
		SSLMode: sslMode,
	}, nil
}

func databaseFromForm(form DatabaseForm, base *agentconfig.AgentConfig) (string, error) {
	if err := validateHost(form.Host, "database.host"); err != nil {
		return "", err
	}
	if form.Port < 1 || form.Port > 65535 {
		return "", fieldError("database.port", "out_of_range", "数据库端口必须为 1..65535")
	}
	if form.Database == "" || strings.ContainsAny(form.Database, "\x00/\\") {
		return "", fieldError("database.database", "invalid", "数据库名称无效")
	}
	if strings.ContainsRune(form.User, '\x00') {
		return "", fieldError("database.user", "invalid", "数据库用户无效")
	}
	if err := validateSSLMode(form.SSLMode); err != nil {
		return "", err
	}
	password := form.Password
	passwordStored := form.ReplacePassword && form.Password != ""
	if !form.ReplacePassword && base != nil && base.PGDSN != "" {
		baseURL, err := parsePostgresURL(base.PGDSN)
		if err != nil {
			return "", fieldError("database", "invalid_base", "现有数据库配置无效")
		}
		if baseURL.User != nil {
			password, passwordStored = baseURL.User.Password()
		}
	}
	u := &url.URL{Scheme: "postgres", Host: net.JoinHostPort(form.Host, strconv.Itoa(form.Port)), Path: "/" + form.Database}
	if form.User != "" {
		if passwordStored {
			u.User = url.UserPassword(form.User, password)
		} else {
			u.User = url.User(form.User)
		}
	}
	query := url.Values{}
	if form.SSLMode != "" {
		query.Set("sslmode", form.SSLMode)
	}
	u.RawQuery = query.Encode()
	return u.String(), nil
}

func parsePostgresURL(dsn string) (*url.URL, error) {
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" {
		return nil, fieldError("database", "invalid_dsn", "数据库连接配置无效")
	}
	for key := range u.Query() {
		if key != "sslmode" {
			return nil, fieldError("database", "unsupported_option", "数据库连接包含不支持的选项")
		}
	}
	return u, nil
}

func splitHostPort(value, hostField, portField string) (string, int, error) {
	host, portText, err := net.SplitHostPort(value)
	if err != nil {
		return "", 0, fieldError(hostField, "invalid_address", "主机和端口格式无效")
	}
	if err := validateHost(host, hostField); err != nil {
		return "", 0, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fieldError(portField, "out_of_range", "端口必须为 1..65535")
	}
	return host, port, nil
}

func validateHost(host, field string) error {
	if host == "" || strings.TrimSpace(host) != host || strings.ContainsAny(host, "/\\@?#[]\x00") {
		return fieldError(field, "invalid_host", "主机名无效")
	}
	for _, r := range host {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return fieldError(field, "invalid_host", "主机名无效")
		}
	}
	if net.ParseIP(host) != nil {
		return nil
	}
	if strings.Contains(host, ":") || invalidNumericAddress(host) {
		return fieldError(field, "invalid_host", "主机名无效")
	}
	dnsName := strings.TrimSuffix(host, ".")
	if dnsName == "" || len(dnsName) > 253 {
		return fieldError(field, "invalid_host", "主机名无效")
	}
	for _, label := range strings.Split(dnsName, ".") {
		if len(label) < 1 || len(label) > 63 || !isASCIIAlphaNumeric(label[0]) || !isASCIIAlphaNumeric(label[len(label)-1]) {
			return fieldError(field, "invalid_host", "主机名无效")
		}
		for i := 1; i < len(label)-1; i++ {
			if !isASCIIAlphaNumeric(label[i]) && label[i] != '-' {
				return fieldError(field, "invalid_host", "主机名无效")
			}
		}
	}
	return nil
}

func invalidNumericAddress(host string) bool {
	for _, r := range host {
		if (r < '0' || r > '9') && r != '.' {
			return false
		}
	}
	return strings.Contains(host, ".")
}

func isASCIIAlphaNumeric(value byte) bool {
	return value >= 'a' && value <= 'z' ||
		value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9'
}

func validateSSLMode(mode string) error {
	switch mode {
	case "", "disable", "allow", "prefer", "require", "verify-ca", "verify-full":
		return nil
	default:
		return fieldError("database.sslMode", "unsupported", "SSL 模式无效")
	}
}

func fieldError(field, code, message string) *FieldError {
	return &FieldError{Field: field, Code: code, Message: message}
}
