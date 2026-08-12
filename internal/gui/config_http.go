package gui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"dedup/internal/config"
)

type guiConfigStore interface {
	Load() (GUIConfigSnapshot, error)
	Save(context.Context, *config.GUIConfig) (GUIConfigSaveResult, error)
}

type guiConfigErrorResponse struct {
	Error  string              `json:"error"`
	Fields []config.FieldError `json:"fields,omitempty"`
}

type configHTTP struct {
	config guiConfigStore
}

func newConfigHTTP(config guiConfigStore) http.Handler {
	return configHTTP{config: config}
}

func (handler configHTTP) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodGet:
		handler.handleGet(response)
	case http.MethodPut:
		handler.handlePut(response, request)
	default:
		response.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (api *API) SetConfigService(service guiConfigStore) {
	api.config = service
}

func (api *API) handleConfigGet(response http.ResponseWriter, _ *http.Request) {
	configHTTP{config: api.config}.handleGet(response)
}

func (handler configHTTP) handleGet(response http.ResponseWriter) {
	if handler.config == nil {
		writeJSON(response, http.StatusServiceUnavailable, guiConfigErrorResponse{Error: "config_unavailable"})
		return
	}
	snapshot, err := handler.config.Load()
	if err != nil {
		writeJSON(response, http.StatusInternalServerError, guiConfigErrorResponse{Error: "config_read_failed"})
		return
	}
	writeJSON(response, http.StatusOK, snapshot)
}

func (api *API) handleConfigPut(response http.ResponseWriter, request *http.Request) {
	configHTTP{config: api.config}.handlePut(response, request)
}

func (handler configHTTP) handlePut(response http.ResponseWriter, request *http.Request) {
	if handler.config == nil {
		writeJSON(response, http.StatusServiceUnavailable, guiConfigErrorResponse{Error: "config_unavailable"})
		return
	}
	var cfg config.GUIConfig
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		writeJSON(response, http.StatusBadRequest, guiConfigErrorResponse{Error: "invalid_request"})
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeJSON(response, http.StatusBadRequest, guiConfigErrorResponse{Error: "invalid_request"})
		return
	}

	result, err := handler.config.Save(request.Context(), &cfg)
	if err != nil {
		var validationErr *config.GUIValidationError
		if errors.As(err, &validationErr) {
			writeJSON(response, http.StatusBadRequest, guiConfigErrorResponse{
				Error:  "config_invalid",
				Fields: validationErr.Fields,
			})
			return
		}
		writeJSON(response, http.StatusInternalServerError, guiConfigErrorResponse{Error: "config_save_failed"})
		return
	}
	writeJSON(response, http.StatusOK, result)
}
