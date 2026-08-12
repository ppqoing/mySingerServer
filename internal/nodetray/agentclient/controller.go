package agentclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"sync"

	agentconfig "dedup/internal/config"
	"dedup/internal/machineid"
	"dedup/internal/nodectl"
	trayconfig "dedup/internal/nodetray/config"
	"dedup/internal/proto"
)

type Controller struct {
	endpoint        string
	pendingEndpoint string
	token           string
	machineID       string

	mu     sync.Mutex
	client *Client
	base   *agentconfig.AgentConfig
}

type ConfigSaveResult struct {
	SHA256          string
	RestartRequired bool
}

func NewController(configuredEndpoint, token, machineID string) (*Controller, error) {
	endpoint, err := LoopbackEndpoint(configuredEndpoint)
	if err != nil || token == "" || !machineid.Valid(machineID) {
		return nil, errors.New("agent_controller_unavailable")
	}
	return &Controller{endpoint: endpoint, token: token, machineID: machineID}, nil
}

func LoopbackEndpoint(configuredEndpoint string) (string, error) {
	_, port, err := net.SplitHostPort(configuredEndpoint)
	if err != nil {
		return "", errors.New("agent_controller_unavailable")
	}
	value, err := strconv.ParseUint(port, 10, 16)
	if err != nil || value == 0 {
		return "", errors.New("agent_controller_unavailable")
	}
	return net.JoinHostPort("127.0.0.1", port), nil
}

func (c *Controller) Status(ctx context.Context) (nodectl.Status, error) {
	var payload proto.LocalStatusGetResponse
	if err := c.call(ctx, proto.LocalOperationStatusGet, nil, &payload); err != nil {
		return nodectl.Status{}, err
	}
	if err := payload.Status.Validate(); err != nil || payload.Status.Component != nodectl.ComponentAgent ||
		payload.Status.MachineID != c.machineID || payload.Status.ConfigSHA256 == "" {
		return nodectl.Status{}, errors.New("agent_status_invalid")
	}
	return payload.Status, nil
}

func (c *Controller) Shutdown(ctx context.Context) error {
	var payload proto.LocalShutdownResponse
	if err := c.call(ctx, proto.LocalOperationShutdown, nil, &payload); err != nil {
		return err
	}
	if !payload.Accepted {
		return errors.New("agent_shutdown_rejected")
	}
	c.PromotePendingEndpoint()
	return nil
}

func (c *Controller) LoadAgentForm(ctx context.Context) (trayconfig.AgentForm, error) {
	var payload proto.LocalConfigGetResponse
	if err := c.call(ctx, proto.LocalOperationConfigGet, nil, &payload); err != nil {
		return trayconfig.AgentForm{}, err
	}
	cfg, err := decodeCanonicalAgent(payload.CanonicalJSON)
	if err != nil {
		return trayconfig.AgentForm{}, errors.New("agent_config_invalid")
	}
	form, err := trayconfig.AgentToForm(cfg)
	if err != nil {
		return trayconfig.AgentForm{}, errors.New("agent_config_invalid")
	}
	c.mu.Lock()
	c.base = cfg
	c.mu.Unlock()
	return form, nil
}

func (c *Controller) ValidateAgentForm(ctx context.Context, value trayconfig.AgentForm) []trayconfig.FieldError {
	canonical, err := c.canonicalFromForm(value)
	if err != nil {
		return []trayconfig.FieldError{{Field: "agent", Code: "invalid", Message: "Agent 配置无效"}}
	}
	var payload proto.LocalConfigValidateResponse
	if err := c.call(ctx, proto.LocalOperationConfigValidate, proto.LocalConfigRequest{CanonicalJSON: canonical}, &payload); err != nil || !payload.Valid {
		return []trayconfig.FieldError{{Field: "agent", Code: "invalid", Message: "Agent 配置无效"}}
	}
	return nil
}

func (c *Controller) SaveAgentForm(ctx context.Context, value trayconfig.AgentForm) (ConfigSaveResult, error) {
	canonical, err := c.canonicalFromForm(value)
	if err != nil {
		return ConfigSaveResult{}, errors.New("agent_config_invalid")
	}
	var payload proto.LocalConfigSaveResponse
	if err := c.call(ctx, proto.LocalOperationConfigSave, proto.LocalConfigRequest{CanonicalJSON: canonical}, &payload); err != nil {
		return ConfigSaveResult{}, err
	}
	pendingEndpoint, err := LoopbackEndpoint(net.JoinHostPort(value.ListenHost, strconv.Itoa(value.ListenPort)))
	if err != nil {
		return ConfigSaveResult{}, errors.New("agent_config_invalid")
	}
	cfg, err := decodeCanonicalAgent(canonical)
	c.mu.Lock()
	if err == nil {
		c.base = cfg
	}
	c.pendingEndpoint = pendingEndpoint
	c.mu.Unlock()
	return ConfigSaveResult{SHA256: payload.SHA256, RestartRequired: payload.RestartRequired}, nil
}

// PromotePendingEndpoint makes the endpoint from the most recent successful
// save active for future dials. It also retires any connection to the old
// Agent, so a successful shutdown can never be followed by a stale call.
func (c *Controller) PromotePendingEndpoint() {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.pendingEndpoint != "" {
		c.endpoint = c.pendingEndpoint
		c.pendingEndpoint = ""
	}
	client := c.client
	c.client = nil
	c.mu.Unlock()
	if client != nil {
		_ = client.Close()
	}
}

func (c *Controller) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	client := c.client
	c.client = nil
	c.mu.Unlock()
	if client != nil {
		return client.Close()
	}
	return nil
}

func (c *Controller) call(ctx context.Context, operation string, request, response any) error {
	if c == nil {
		return ErrAgentDisconnected
	}
	client, err := c.connected(ctx)
	if err != nil {
		return err
	}
	err = client.Call(ctx, operation, request, response)
	if errors.Is(err, ErrAgentDisconnected) {
		c.mu.Lock()
		if c.client == client {
			c.client = nil
		}
		c.mu.Unlock()
		_ = client.Close()
	}
	return err
}

func (c *Controller) connected(ctx context.Context) (*Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil {
		return c.client, nil
	}
	client, err := Dial(ctx, c.endpoint, c.token, c.machineID)
	if err != nil {
		return nil, err
	}
	c.client = client
	return client, nil
}

func (c *Controller) canonicalFromForm(value trayconfig.AgentForm) ([]byte, error) {
	c.mu.Lock()
	base := c.base
	c.mu.Unlock()
	if base == nil {
		return nil, errors.New("agent config base unavailable")
	}
	cfg, err := trayconfig.AgentFromForm(value, base)
	if err != nil {
		return nil, err
	}
	return canonicalAgentJSON(cfg)
}

func decodeCanonicalAgent(payload []byte) (*agentconfig.AgentConfig, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var cfg agentconfig.AgentConfig
	if err := decoder.Decode(&cfg); err != nil {
		return nil, err
	}
	if decoder.More() {
		return nil, errors.New("trailing Agent config")
	}
	canonical, err := canonicalAgentJSON(&cfg)
	if err != nil || !bytes.Equal(canonical, payload) {
		return nil, errors.New("Agent config is not canonical")
	}
	return &cfg, nil
}

func canonicalAgentJSON(cfg *agentconfig.AgentConfig) ([]byte, error) {
	payload, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(payload, '\n'), nil
}
