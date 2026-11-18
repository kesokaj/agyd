package api

import (
	"net/http"

	"agyd/internal/acp"
)

// modelsResponse lists the values agy offers, under the API names (D1).
type modelsResponse struct {
	Models         []acp.ConfigValue `json:"models"`
	ThinkingLevels []acp.ConfigValue `json:"thinkingLevels"`
	Modes          []acp.ConfigValue `json:"modes"`
}

// handleModels answers from the configOptions agy last reported for any
// conversation; before the first one it is 503 UNAVAILABLE (D20).
func (s *server) handleModels(w http.ResponseWriter, _ *http.Request) error {
	opts, err := s.mgr.Models()
	if err != nil {
		return err
	}
	res := modelsResponse{Models: []acp.ConfigValue{}, ThinkingLevels: []acp.ConfigValue{}, Modes: []acp.ConfigValue{}}
	for _, o := range opts {
		switch o.ID {
		case acp.ConfigModel:
			res.Models = append(res.Models, o.Options...)
		case acp.ConfigThinking:
			res.ThinkingLevels = append(res.ThinkingLevels, o.Options...)
		case acp.ConfigMode:
			res.Modes = append(res.Modes, o.Options...)
		}
	}
	writeJSON(w, http.StatusOK, res)
	return nil
}

func (s *server) handleGetDefaults(w http.ResponseWriter, _ *http.Request) error {
	writeJSON(w, http.StatusOK, s.mgr.Defaults())
	return nil
}

// handlePutDefaults merges the given settings into the running defaults and
// writes them to config.env (D7, D13). Values are validated before the file
// is touched; omitted fields keep their value.
func (s *server) handlePutDefaults(w http.ResponseWriter, r *http.Request) error {
	var req settingsRequest
	if err := readJSON(r, &req, false); err != nil {
		return err
	}
	d, err := s.mgr.SetDefaults(req.settings(), true)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, d)
	return nil
}
