package orchestrator

import (
	"errors"
	"net"
)

type Config struct {
	Root, DBPath, Bind, DeviceID, DeviceToken, EventID string
	MaxUploadBytes                                     int64
	MaxPixels                                          int64
	MaxAttempts                                        int
}

func DefaultConfig() Config {
	return Config{Root: "data", Bind: "127.0.0.1:8090", DeviceID: "camera_01", DeviceToken: "local-development-token", EventID: "local-event", MaxUploadBytes: 25 << 20, MaxPixels: 50_000_000, MaxAttempts: 3}
}

func (c Config) Validate() error {
	if c.Root == "" {
		return errors.New("invalid storage root")
	}
	if c.Bind == "" || c.DeviceID == "" || c.DeviceToken == "" || c.EventID == "" {
		return errors.New("missing required local configuration")
	}
	host, _, err := net.SplitHostPort(c.Bind)
	if err != nil || host == "" {
		return errors.New("bind must use a local host and port")
	}
	if c.MaxUploadBytes <= 0 || c.MaxPixels <= 0 || c.MaxAttempts <= 0 {
		return errors.New("limits must be positive")
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
