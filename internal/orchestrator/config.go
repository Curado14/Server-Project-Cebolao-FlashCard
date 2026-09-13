package orchestrator

import (
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Root, DBPath, Bind, DeviceID, DeviceToken, EventID string
	InstanceID                                         string
	MaxUploadBytes                                     int64
	MaxPixels                                          int64
	MaxAttempts                                        int
	MinFreeBytes                                       uint64
	AllowNetworkBind                                   bool
}

func DefaultConfig() Config {
	return Config{Root: "data", Bind: "127.0.0.1:8090", DeviceID: "camera_01", DeviceToken: "local-development-token", EventID: "local-event", InstanceID: defaultInstanceID(), MaxUploadBytes: 25 << 20, MaxPixels: 50_000_000, MaxAttempts: 3, MinFreeBytes: 1 << 30}
}

func ConfigFromEnv(base Config) (Config, error) {
	cfg := base
	if value := os.Getenv("PHOTOEDGE_ROOT"); value != "" {
		cfg.Root = value
	}
	if value := os.Getenv("PHOTOEDGE_DB_PATH"); value != "" {
		cfg.DBPath = value
	}
	if value := os.Getenv("PHOTOEDGE_BIND"); value != "" {
		cfg.Bind = value
		cfg.AllowNetworkBind = true
	}
	if value := os.Getenv("PHOTOEDGE_DEVICE_ID"); value != "" {
		cfg.DeviceID = value
	}
	if value := os.Getenv("PHOTOEDGE_TOKEN"); value != "" {
		cfg.DeviceToken = value
	}
	if value := os.Getenv("PHOTOEDGE_EVENT_ID"); value != "" {
		cfg.EventID = value
	}
	if value := os.Getenv("PHOTOEDGE_INSTANCE_ID"); value != "" {
		cfg.InstanceID = safeIdentifier(value, "local-instance")
	}
	if value := os.Getenv("PHOTOEDGE_MAX_UPLOAD_BYTES"); value != "" {
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return Config{}, errors.New("PHOTOEDGE_MAX_UPLOAD_BYTES must be an integer")
		}
		cfg.MaxUploadBytes = n
	}
	if value := os.Getenv("PHOTOEDGE_MAX_PIXELS"); value != "" {
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return Config{}, errors.New("PHOTOEDGE_MAX_PIXELS must be an integer")
		}
		cfg.MaxPixels = n
	}
	if value := os.Getenv("PHOTOEDGE_MAX_ATTEMPTS"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil {
			return Config{}, errors.New("PHOTOEDGE_MAX_ATTEMPTS must be an integer")
		}
		cfg.MaxAttempts = n
	}
	if value := os.Getenv("PHOTOEDGE_MIN_FREE_BYTES"); value != "" {
		n, err := strconv.ParseUint(value, 10, 64)
		if err != nil || n == 0 {
			return Config{}, errors.New("PHOTOEDGE_MIN_FREE_BYTES must be a positive integer")
		}
		cfg.MinFreeBytes = n
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if c.Root == "" {
		return errors.New("invalid storage root")
	}
	if c.Bind == "" || c.DeviceID == "" || c.DeviceToken == "" || c.EventID == "" {
		return errors.New("missing required local configuration")
	}
	if c.InstanceID == "" {
		return errors.New("missing instance id")
	}
	host, _, err := net.SplitHostPort(c.Bind)
	if err != nil || host == "" {
		return errors.New("bind must use a local host and port")
	}
	if c.MaxUploadBytes <= 0 || c.MaxPixels <= 0 || c.MaxAttempts <= 0 || c.MinFreeBytes == 0 {
		return errors.New("limits must be positive")
	}
	if c.AllowNetworkBind {
		return nil
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("bind must be loopback-only")
	}
	return nil
}

func defaultInstanceID() string {
	host, err := os.Hostname()
	if err != nil {
		return "local-instance"
	}
	return safeIdentifier(host, "local-instance")
}

func safeIdentifier(value, fallback string) string {
	value = strings.TrimSpace(value)
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '_' || r == '-' || r == '.':
			b.WriteRune(r)
		}
		if b.Len() >= 80 {
			break
		}
	}
	if b.Len() == 0 {
		return fallback
	}
	return b.String()
}
