package flows

import (
	"errors"
	"slices"

	"github.com/braedonsaunders/ether/internal/events"
)

// RedactGraphActions returns a graph view with action credentials masked.
func RedactGraphActions(g Graph, actions ActionRegistry) Graph {
	g.Nodes = slices.Clone(g.Nodes)
	for i, node := range g.Nodes {
		if node.Kind != NodeAction {
			continue
		}
		cfg, err := DecodeConfig[ActionNodeConfig](node)
		if err != nil {
			g.Nodes[i].Config = nil
			continue
		}
		var spec ActionSpec
		var known bool
		if actions != nil {
			spec, known = actions.Spec(cfg.ActionType)
		}
		if known {
			cfg.Params = redactActionParams(spec, cfg.Params)
		} else {
			cfg.Params = nil
		}
		g.Nodes[i].Config = marshalJSONRaw(cfg)
	}
	return g
}

var errMissingActionSecret = errors.New("flows: a retained action credential is missing")

type actionSecretMarker struct {
	index int
	cfg   ActionNodeConfig
	keys  []string
}

func actionSecretMarkers(g Graph, actions ActionRegistry) []actionSecretMarker {
	if actions == nil {
		return nil
	}
	var markers []actionSecretMarker
	for i, node := range g.Nodes {
		if node.Kind != NodeAction {
			continue
		}
		cfg, err := DecodeConfig[ActionNodeConfig](node)
		if err != nil {
			continue
		}
		spec, ok := actions.Spec(cfg.ActionType)
		if !ok {
			continue
		}
		var keys []string
		for _, param := range spec.Params {
			if param.Secret && cfg.Params[param.Key] == events.RedactedValue {
				keys = append(keys, param.Key)
			}
		}
		if len(keys) > 0 {
			markers = append(markers, actionSecretMarker{index: i, cfg: cfg, keys: keys})
		}
	}
	return markers
}

func restoreActionSecrets(next, previous Graph, markers []actionSecretMarker) (Graph, error) {
	previousActions := make(map[string]ActionNodeConfig)
	for _, node := range previous.Nodes {
		if node.Kind == NodeAction {
			if cfg, err := DecodeConfig[ActionNodeConfig](node); err == nil {
				previousActions[node.ID] = cfg
			}
		}
	}
	next.Nodes = slices.Clone(next.Nodes)
	for _, marker := range markers {
		node := &next.Nodes[marker.index]
		prior := previousActions[node.ID]
		if prior.ActionType != marker.cfg.ActionType {
			return Graph{}, errMissingActionSecret
		}
		for _, key := range marker.keys {
			value := prior.Params[key]
			if value == nil || value == "" || value == events.RedactedValue {
				return Graph{}, errMissingActionSecret
			}
			marker.cfg.Params[key] = value
		}
		node.Config = marshalJSONRaw(marker.cfg)
	}
	return next, nil
}
