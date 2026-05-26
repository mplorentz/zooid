package zooid

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
)

type APIHandler struct {
	whitelist map[string]bool
	mux       http.Handler
}

func NewAPIHandler() *APIHandler {
	whitelist := make(map[string]bool)
	for _, pubkey := range Split(Env("API_WHITELIST"), ",") {
		pubkey = strings.TrimSpace(pubkey)
		if pubkey != "" {
			whitelist[pubkey] = true
		}
	}

	api := &APIHandler{
		whitelist: whitelist,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /relay/{id}", api.auth(api.createRelay))
	mux.HandleFunc("PUT /relay/{id}", api.auth(api.putRelay))
	mux.HandleFunc("PATCH /relay/{id}", api.auth(api.patchRelay))
	mux.HandleFunc("DELETE /relay/{id}", api.auth(api.deleteRelay))
	mux.HandleFunc("GET /relay/{id}/members", api.auth(api.listRelayMembers))

	api.mux = mux

	return api
}

func (api *APIHandler) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pubkey, err := validateNIP98Auth(r)
		if err != nil {
			writeError(w, http.StatusUnauthorized, err.Error())
			return
		}
		if !api.whitelist[pubkey.Hex()] {
			writeError(w, http.StatusForbidden, "pubkey not in whitelist")
			return
		}
		next(w, r)
	}
}

func (api *APIHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	api.mux.ServeHTTP(w, r)
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// Relay CRUD

func (api *APIHandler) configFromRequest(path string, r *http.Request) (*Config, error) {
	r.Body = http.MaxBytesReader(nil, r.Body, 1024*1024)
	defer r.Body.Close()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read body: %w", err)
	}

	return LoadConfigFromJson(path, body)
}

func (api *APIHandler) patchFromRequest(r *http.Request) (map[string]interface{}, error) {
	r.Body = http.MaxBytesReader(nil, r.Body, 1024*1024)
	defer r.Body.Close()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read body: %w", err)
	}

	var patch map[string]interface{}
	if err := json.Unmarshal(body, &patch); err != nil {
		return nil, fmt.Errorf("invalid json config: %w", err)
	}

	return patch, nil
}

func (api *APIHandler) checkDuplicateSchemaOrHost(config *Config, excludeFilename string) error {
	entries, err := os.ReadDir(Env("CONFIG"))
	if err != nil {
		return fmt.Errorf("failed to read config directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == excludeFilename || !strings.HasSuffix(entry.Name(), ".toml") {
			continue
		}

		path := ConfigPathFromName(entry.Name())

		if existing, err := LoadConfigFromPath(path); err == nil {
			if existing.Schema == config.Schema {
				return fmt.Errorf("schema %q is already in use", config.Schema)
			}
			if existing.Host == config.Host {
				return fmt.Errorf("host %q is already in use", config.Host)
			}
		}
	}

	return nil
}

// Create relay

func (api *APIHandler) createRelay(w http.ResponseWriter, r *http.Request) {
	name := ConfigNameFromId(r.PathValue("id"))
	path := ConfigPathFromName(name)
	if _, err := os.Stat(path); err == nil {
		writeError(w, http.StatusConflict, "relay with this id already exists")
		return
	}

	config, err := api.configFromRequest(path, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := api.checkDuplicateSchemaOrHost(config, ""); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	if err := config.Save(); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to write config: %v", err))
		return
	}

	writeJSON(w, http.StatusCreated, map[string]string{"message": "relay created successfully"})
}

// Put relay

func (api *APIHandler) putRelay(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	name := ConfigNameFromId(id)
	path := ConfigPathFromName(name)
	if _, err := os.Stat(path); err != nil {
		writeError(w, http.StatusConflict, "relay not found")
		return
	}

	config, err := api.configFromRequest(path, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := api.checkDuplicateSchemaOrHost(config, name); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	if err := config.Save(); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to write config: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "relay updated successfully"})
}

// Patch relay

func (api *APIHandler) patchRelay(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	name := ConfigNameFromId(id)
	path := ConfigPathFromName(name)
	if _, err := os.Stat(path); err != nil {
		writeError(w, http.StatusConflict, "relay not found")
		return
	}

	config, err := LoadConfigFromPath(path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to read existing config: %v", err))
		return
	}

	patch, err := api.patchFromRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := api.applyPatch(config, patch); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := config.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := api.checkDuplicateSchemaOrHost(config, name); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	if err := config.Save(); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to write config: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "relay patched successfully"})
}

func (api *APIHandler) applyPatch(config *Config, patch map[string]interface{}) error {
	// Convert config to map for merging
	configJSON, _ := json.Marshal(config)
	var configMap map[string]interface{}
	json.Unmarshal(configJSON, &configMap)

	// Merge patch
	merged := deepMerge(configMap, patch)

	// Convert back to a new config (don't modify original until validation passes)
	mergedJSON, _ := json.Marshal(merged)
	var patched Config
	if err := json.Unmarshal(mergedJSON, &patched); err != nil {
		return err
	}

	// Copy patched values to original config
	*config = patched
	return nil
}

func deepMerge(base, patch map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{})

	for k, v := range base {
		result[k] = v
	}

	for k, v := range patch {
		if v == nil {
			delete(result, k)
		} else if patchMap, ok := v.(map[string]interface{}); ok {
			if baseMap, ok := base[k].(map[string]interface{}); ok {
				result[k] = deepMerge(baseMap, patchMap)
			} else {
				result[k] = v
			}
		} else {
			result[k] = v
		}
	}

	return result
}

// Delete relay

func (api *APIHandler) deleteRelay(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	name := ConfigNameFromId(id)
	path := ConfigPathFromName(name)
	if _, err := os.Stat(path); err != nil {
		writeError(w, http.StatusConflict, "relay not found")
		return
	}

	if err := os.Remove(path); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to delete config: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "relay deleted successfully"})
}

// Relay members endpoint

func (api *APIHandler) listRelayMembers(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	name := ConfigNameFromId(id)
	members, err := api.resolveRelayMembers(name)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, "relay not found")
		} else {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to load relay members: %v", err))
		}
		return
	}

	writeJSON(w, http.StatusOK, map[string][]string{"members": members})
}

func (api *APIHandler) resolveRelayMembers(name string) ([]string, error) {
	instancesMux.RLock()
	instance, exists := instancesByName[name]
	instancesMux.RUnlock()

	if exists {
		return collectMembers(instance.Management), nil
	}

	path := ConfigPathFromName(name)
	config, err := LoadConfigFromPath(path)
	if err != nil {
		return nil, err
	}

	events := &EventStore{
		Config: config,
		Schema: &Schema{Name: config.Schema},
	}

	if err := events.Init(); err != nil {
		return nil, fmt.Errorf("failed to init event store: %w", err)
	}

	management := &ManagementStore{
		Config: config,
		Events: events,
	}

	return collectMembers(management), nil
}

func collectMembers(management *ManagementStore) []string {
	memberSet := make(map[string]struct{})
	for _, pubkey := range management.GetMembers() {
		memberSet[pubkey.Hex()] = struct{}{}
	}
	members := Keys(memberSet)
	sort.Strings(members)
	return members
}
