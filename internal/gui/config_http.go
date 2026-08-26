package gui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"dedup/internal/config"
)

type guiConfigStore interface {
	Load() (GUIConfigSnapshot, error)
	Save(context.Context, *config.GUIConfig) (GUIConfigSaveResult, error)
}

type guiRestartCoordinator interface {
	Begin() bool
	End()
	Pending() bool
	Prepare(*config.GUIConfig) (string, error)
	Commit()
	Abort()
}

type guiRestartProvider interface {
	RestartCoordinator() guiRestartCoordinator
}

type guiConfigErrorResponse struct {
	Error  string              `json:"error"`
	Fields []config.FieldError `json:"fields,omitempty"`
}

type guiConfigRestartErrorResponse struct {
	GUIConfigSaveResult
	Error string `json:"error"`
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

func (handler configHTTP) handlePut(response http.ResponseWriter, request *http.Request) {
	if handler.config == nil {
		writeJSON(response, http.StatusServiceUnavailable, guiConfigErrorResponse{Error: "config_unavailable"})
		return
	}
	restart := restartCoordinatorFor(handler.config)
	if restart != nil {
		if !restart.Begin() {
			writeJSON(response, http.StatusConflict, guiConfigErrorResponse{Error: "restart_in_progress"})
			return
		}
		defer restart.End()
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
	if result.RestartRequired && restart != nil {
		recoveryURL, prepareErr := restart.Prepare(&cfg)
		if prepareErr != nil {
			restart.Abort()
			writeJSON(response, http.StatusInternalServerError, guiConfigRestartErrorResponse{
				GUIConfigSaveResult: result,
				Error:               "restart_launch_failed",
			})
			return
		}
		result.Restarting = true
		result.RecoveryURL = recoveryURL
		if request.Context().Err() != nil {
			restart.Abort()
			return
		}
		if err := writeGUIRestartResponse(response, result); err != nil {
			restart.Abort()
			return
		}
		if request.Context().Err() != nil {
			restart.Abort()
			return
		}
		restart.Commit()
		return
	}
	writeJSON(response, http.StatusOK, result)
}

func writeGUIRestartResponse(response http.ResponseWriter, result GUIConfigSaveResult) error {
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(response).Encode(result); err != nil {
		return fmt.Errorf("write restart response: %w", err)
	}
	if err := http.NewResponseController(response).Flush(); err != nil {
		return fmt.Errorf("flush restart response: %w", err)
	}
	return nil
}

func restartCoordinatorFor(store guiConfigStore) guiRestartCoordinator {
	provider, ok := store.(guiRestartProvider)
	if !ok {
		return nil
	}
	return provider.RestartCoordinator()
}
