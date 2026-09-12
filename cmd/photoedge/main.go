package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"photoedge/internal/orchestrator"
)

func main() {
	cfg := orchestrator.DefaultConfig()
	if err := cfg.Validate(); err != nil {
		log.Fatal(err)
	}
	s, err := orchestrator.Open(cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()
	s.StartWorkers(context.Background(), 1)
	log.Printf("photo edge listening on %s", cfg.Bind)
	log.Fatal(http.ListenAndServe(cfg.Bind, s.Handler()))
	_ = os.Args
}
