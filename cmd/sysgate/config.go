package main

import (
	"os"
	"strconv"
	"time"
)

type config struct {
	listen       string
	driver       string
	runscBin     string
	root         string
	rootfs       string
	network      string
	poolSize     int
	maxSandboxes int
	defaultTTL   time.Duration
	execTimeout  time.Duration
}

func loadConfig() config {
	return config{
		listen:       env("SYSGATE_LISTEN", ":8080"),
		driver:       env("SYSGATE_DRIVER", "fake"),
		runscBin:     env("SYSGATE_RUNSC_BIN", "runsc"),
		root:         env("SYSGATE_ROOT", "/var/lib/sysgate"),
		rootfs:       env("SYSGATE_ROOTFS", ""),
		network:      env("SYSGATE_NETWORK", "none"),
		poolSize:     envInt("SYSGATE_POOL_SIZE", 4),
		maxSandboxes: envInt("SYSGATE_MAX_SANDBOXES", 256),
		defaultTTL:   envDuration("SYSGATE_DEFAULT_TTL", 10*time.Minute),
		execTimeout:  envDuration("SYSGATE_EXEC_TIMEOUT", 30*time.Second),
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	v, err := strconv.Atoi(os.Getenv(key))
	if err != nil {
		return def
	}
	return v
}

func envDuration(key string, def time.Duration) time.Duration {
	v, err := time.ParseDuration(os.Getenv(key))
	if err != nil {
		return def
	}
	return v
}
