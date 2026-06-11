// Command rest-demo is a tiny in-memory orders API used as the live target
// for gateway federation demos. It serves its own OpenAPI spec at
// /openapi.yaml so the binary is fully self-contained.
//
// Run with: go run ./examples/rest-demo
// Override the listen address with RUNTIME_DEMO_ADDR (default :9000).
package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
)

//go:embed openapi.yaml
var openapiSpec []byte

// Order is a single order in the store.
type Order struct {
	ID     string `json:"id"`
	Item   string `json:"item"`
	Qty    int    `json:"qty"`
	Status string `json:"status"`
}

type store struct {
	mu     sync.Mutex
	orders map[string]Order
	nextID int
}

func newStore() *store {
	return &store{
		orders: map[string]Order{
			"o1": {ID: "o1", Item: "widget", Qty: 2, Status: "open"},
			"o2": {ID: "o2", Item: "gadget", Qty: 1, Status: "shipped"},
			"o3": {ID: "o3", Item: "sprocket", Qty: 5, Status: "open"},
		},
		nextID: 4,
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func main() {
	addr := os.Getenv("RUNTIME_DEMO_ADDR")
	if addr == "" {
		addr = ":9000"
	}
	s := newStore()
	mux := http.NewServeMux()

	mux.HandleFunc("GET /orders", func(w http.ResponseWriter, r *http.Request) {
		status := r.URL.Query().Get("status")
		s.mu.Lock()
		out := make([]Order, 0, len(s.orders))
		for _, o := range s.orders {
			if status == "" || o.Status == status {
				out = append(out, o)
			}
		}
		s.mu.Unlock()
		// Stable order for demo output.
		for i := range out {
			for j := i + 1; j < len(out); j++ {
				if out[j].ID < out[i].ID {
					out[i], out[j] = out[j], out[i]
				}
			}
		}
		writeJSON(w, http.StatusOK, out)
	})

	mux.HandleFunc("GET /orders/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		o, ok := s.orders[r.PathValue("id")]
		s.mu.Unlock()
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such order"})
			return
		}
		writeJSON(w, http.StatusOK, o)
	})

	mux.HandleFunc("POST /orders", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Item string `json:"item"`
			Qty  int    `json:"qty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Item == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
			return
		}
		s.mu.Lock()
		o := Order{ID: fmt.Sprintf("o%d", s.nextID), Item: in.Item, Qty: in.Qty, Status: "open"}
		s.nextID++
		s.orders[o.ID] = o
		s.mu.Unlock()
		writeJSON(w, http.StatusCreated, o)
	})

	mux.HandleFunc("GET /openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(openapiSpec)
	})

	logged := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slog.Info("request", "method", r.Method, "path", r.URL.Path)
		mux.ServeHTTP(w, r)
	})

	slog.Info("rest-demo orders API listening", "addr", addr)
	if err := http.ListenAndServe(addr, logged); err != nil {
		slog.Error("server failed", "err", err)
		os.Exit(1)
	}
}
