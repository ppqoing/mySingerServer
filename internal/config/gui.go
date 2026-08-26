package config

import (
	"encoding/json"
	"fmt"
	"math"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

type FieldError struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type GUIValidationError struct {
	Fields []FieldError `json:"fields"`
}

func (e *GUIValidationError) Error() string {
	fields := make([]string, 0, len(e.Fields))
	for _, field := range e.Fields {
		fields = append(fields, field.Field)
	}
	return "config: invalid fields: " + strings.Join(fields, ", ")
}

type GUIConfig struct {
	ListenAddr  string            `json:"listen_addr"`
	PGDSN       string            `json:"pg_dsn"`
	Agents      []AgentEndpoint   `json:"agents"`
	HeartbeatS  int               `json:"heartbeat_s"`
	FirstScreen FirstScreenConfig `json:"firstscreen"`
	Phase2      Phase2Config      `json:"phase2"`
}

type FirstScreenConfig struct {
	HammingMax            int     `json:"hamming_max"`
	AspectTolerance       float64 `json:"aspect_tolerance"`
	VideoDurationWindowMs int64   `json:"video_duration_window_ms"`
	ImageQualityMin       int     `json:"image_quality_min"`
	ReadPageSize          int     `json:"read_page_size"`
	GroupInsertBatch      int     `json:"group_insert_batch"`
	SHAResolveChunk       int     `json:"sha_resolve_chunk"`
}

type Phase2Config struct {
	PHashPassT2               float64 `json:"phash_pass_t2"`
	PHashPartThreshold        int     `json:"phash_part_threshold"`
	SobelT3                   float64 `json:"sobel_t3"`
	VideoFrames               int     `json:"video_frames"`
	VideoAvgT4                float64 `json:"video_avg_t4"`
	VideoMinPassed            int     `json:"video_min_passed"`
	VideoMinValid             int     `json:"video_min_valid"`
	VideoFileTimeoutS         int     `json:"video_file_timeout_s"`
	VideoFrameCommandTimeoutS int     `json:"video_frame_command_timeout_s"`
	ImageFileTimeoutS         int     `json:"image_file_timeout_s"`
	TaskShardSize             int     `json:"task_shard_size"`
	AutoDispatch              bool    `json:"auto_dispatch"`
}

type AgentEndpoint struct {
	Addr string `json:"addr"`
}

func DefaultGUI() *GUIConfig {
	cfg := defaultGUIOptionalFields()
	cfg.PGDSN = ""
	cfg.Agents = []AgentEndpoint{{Addr: "127.0.0.1:9101"}}
	return cfg
}

func defaultGUIOptionalFields() *GUIConfig {
	return &GUIConfig{
		ListenAddr:  "127.0.0.1:18081",
		HeartbeatS:  15,
		FirstScreen: defaultFirstScreen(),
		Phase2:      defaultPhase2(),
	}
}

func LoadGUI(path string) (*GUIConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := defaultGUIOptionalFields()
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("config: parse: %w", err)
	}
	if err := ValidateGUI(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func ValidateGUI(cfg *GUIConfig) error {
	validation := &guiValidation{}
	if cfg == nil {
		validation.add("config", "required", "配置不能为空")
		return validation.err()
	}

	if cfg.ListenAddr == "" {
		validation.add("listen_addr", "required", "监听地址不能为空")
	} else if !validGUIHostPort(cfg.ListenAddr) {
		validation.add("listen_addr", "invalid_address", "地址必须是 host:port")
	}
	if cfg.PGDSN != "" {
		if _, err := pgxpool.ParseConfig(cfg.PGDSN); err != nil {
			validation.add("pg_dsn", "invalid_dsn", "必须是可解析的 PostgreSQL DSN")
		}
	}
	if cfg.HeartbeatS < 1 {
		validation.add("heartbeat_s", "positive", "心跳间隔必须为正数")
	}
	if len(cfg.Agents) == 0 {
		validation.add("agents", "required", "至少需要一个 Agent")
	}
	seen := make(map[string]bool, len(cfg.Agents))
	for index, endpoint := range cfg.Agents {
		addrField := fmt.Sprintf("agents[%d].addr", index)
		key := strings.ToLower(endpoint.Addr)
		switch {
		case endpoint.Addr == "":
			validation.add(addrField, "required", "Agent 地址不能为空")
		case !validGUIHostPort(endpoint.Addr):
			validation.add(addrField, "invalid_address", "地址必须是 host:port")
		case seen[key]:
			validation.add(addrField, "duplicate", "Agent 地址不能重复")
		default:
			seen[key] = true
		}
	}
	cfg.FirstScreen.collectValidation(validation)
	cfg.Phase2.collectValidation(validation)
	return validation.err()
}

type guiValidation struct {
	fields []FieldError
}

func (v *guiValidation) add(field, code, message string) {
	v.fields = append(v.fields, FieldError{Field: field, Code: code, Message: message})
}

func (v *guiValidation) err() error {
	if len(v.fields) == 0 {
		return nil
	}
	return &GUIValidationError{Fields: v.fields}
}

func validGUIHostPort(value string) bool {
	host, portText, err := net.SplitHostPort(value)
	if err != nil || host == "" || strings.TrimSpace(host) != host {
		return false
	}
	port, err := strconv.Atoi(portText)
	return err == nil && port >= 1 && port <= 65535
}

func defaultPhase2() Phase2Config {
	return Phase2Config{
		PHashPassT2: 0.80, PHashPartThreshold: 10, SobelT3: 0.85,
		VideoFrames: 6, VideoAvgT4: 0.80, VideoMinPassed: 4, VideoMinValid: 4,
		VideoFileTimeoutS: 120, VideoFrameCommandTimeoutS: 20, ImageFileTimeoutS: 30,
		TaskShardSize: 5000, AutoDispatch: true,
	}
}

func defaultFirstScreen() FirstScreenConfig {
	return FirstScreenConfig{
		HammingMax:            31,
		AspectTolerance:       0.10,
		VideoDurationWindowMs: 2000,
		ImageQualityMin:       50,
		ReadPageSize:          50000,
		GroupInsertBatch:      1000,
		SHAResolveChunk:       10000,
	}
}

func (c FirstScreenConfig) collectValidation(validation *guiValidation) {
	if c.HammingMax < 0 || c.HammingMax > 256 {
		validation.add("firstscreen.hamming_max", "out_of_range", "必须在 0 到 256 之间")
	}
	if c.AspectTolerance < 0 || c.AspectTolerance > 1 {
		validation.add("firstscreen.aspect_tolerance", "out_of_range", "必须在 0 到 1 之间")
	}
	if c.VideoDurationWindowMs < 0 {
		validation.add("firstscreen.video_duration_window_ms", "non_negative", "不能为负数")
	}
	if c.ImageQualityMin < 0 || c.ImageQualityMin > 100 {
		validation.add("firstscreen.image_quality_min", "out_of_range", "必须在 0 到 100 之间")
	}
	if c.ReadPageSize < 1 {
		validation.add("firstscreen.read_page_size", "positive", "必须为正数")
	}
	if c.GroupInsertBatch < 1 {
		validation.add("firstscreen.group_insert_batch", "positive", "必须为正数")
	}
	if c.SHAResolveChunk < 1 {
		validation.add("firstscreen.sha_resolve_chunk", "positive", "必须为正数")
	}
}

func (c Phase2Config) validate() error {
	validation := &guiValidation{}
	c.collectValidation(validation)
	return validation.err()
}

func (c Phase2Config) collectValidation(validation *guiValidation) {
	if !validThreshold(c.PHashPassT2) {
		validation.add("phase2.phash_pass_t2", "out_of_range", "必须是 0 到 1 之间的有限数")
	}
	if !validThreshold(c.SobelT3) {
		validation.add("phase2.sobel_t3", "out_of_range", "必须是 0 到 1 之间的有限数")
	}
	if !validThreshold(c.VideoAvgT4) {
		validation.add("phase2.video_avg_t4", "out_of_range", "必须是 0 到 1 之间的有限数")
	}
	if c.PHashPartThreshold < 0 || c.PHashPartThreshold > 64 {
		validation.add("phase2.phash_part_threshold", "out_of_range", "必须在 0 到 64 之间")
	}
	if c.VideoFrames != 6 {
		validation.add("phase2.video_frames", "fixed_value", "当前必须为 6")
	}
	if c.VideoMinPassed < 1 || c.VideoMinPassed > c.VideoFrames {
		validation.add("phase2.video_min_passed", "out_of_range", "必须在 1 到视频帧数之间")
	}
	if c.VideoMinValid < 1 || c.VideoMinValid > c.VideoFrames {
		validation.add("phase2.video_min_valid", "out_of_range", "必须在 1 到视频帧数之间")
	}
	if c.VideoFileTimeoutS < 1 {
		validation.add("phase2.video_file_timeout_s", "positive", "必须为正数")
	}
	if c.VideoFrameCommandTimeoutS < 1 {
		validation.add("phase2.video_frame_command_timeout_s", "positive", "必须为正数")
	}
	if c.ImageFileTimeoutS < 1 {
		validation.add("phase2.image_file_timeout_s", "positive", "必须为正数")
	}
	if c.TaskShardSize < 1 {
		validation.add("phase2.task_shard_size", "positive", "必须为正数")
	}
	if c.TaskShardSize > 5000 {
		validation.add("phase2.task_shard_size", "max_value", "不能超过 5000")
	}
}

func validThreshold(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 1
}
