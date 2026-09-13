package main

import (
	"context"
	"net/http"
	"photoedge/internal/orchestrator"
)

func main() {
	cfg, err := orchestrator.ConfigFromEnv(orchestrator.DefaultConfig())
	if err != nil {
		panic(err)
	}
	if err := cfg.Validate(); err != nil {
		panic(err)
	}
	s, err := orchestrator.Open(cfg)
	if err != nil {
		panic(err)
	}
	defer s.Close()
	s.StartWorkers(context.Background(), 1)
	if err := http.ListenAndServe(cfg.Bind, s.Handler()); err != nil {
		panic(err)
	}
}
