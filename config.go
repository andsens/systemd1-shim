package main

import (
	"encoding/json"
	"os"
	"strconv"
	"time"
)

// Config holds everything environment-specific. All of it comes from env
// vars except the unit->container mapping, which can optionally come from
// a JSON file for larger/less-static setups.
type Config struct {
	PodName        string
	Namespace      string
	KubectlPath    string
	ExecTimeout    time.Duration
	UnitContainers map[string]string
}

func loadConfig() (*Config, error) {
	cfg := &Config{
		PodName:     os.Getenv("POD_NAME"),
		Namespace:   getenvDefault("POD_NAMESPACE", "default"),
		KubectlPath: getenvDefault("KUBECTL_PATH", "kubectl"),
		ExecTimeout: 30 * time.Second,
	}
	if cfg.PodName == "" {
		logf("WARNING: POD_NAME is not set - wire it up via the Downward API " +
			"(fieldRef: metadata.name) in the sidecar's env, see README.md. " +
			"kubectl exec calls will fail until then.")
	}

	if s := os.Getenv("EXEC_TIMEOUT_SECONDS"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			cfg.ExecTimeout = time.Duration(n) * time.Second
		}
	}

	cfg.UnitContainers = map[string]string{}
	if path := os.Getenv("UNIT_CONTAINER_MAP_FILE"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &cfg.UnitContainers); err != nil {
			return nil, err
		}
	}
	// Normalize keys to always carry the ".service" suffix so lookups in
	// KubectlRestarter.containerFor (which always looks up the normalized
	// name) hit regardless of how the user wrote the mapping file.
	normalized := map[string]string{}
	for k, v := range cfg.UnitContainers {
		normalized[normalizeUnitName(k)] = v
	}
	cfg.UnitContainers = normalized

	return cfg, nil
}

func getenvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
