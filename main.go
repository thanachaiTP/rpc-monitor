// rpc-health: เช็ค block height ของ EVM JSON-RPC node (eth_blockNumber)
// - block ยังขยับภายใน STALL_THRESHOLD  -> 200
// - block ไม่ขยับเกิน STALL_THRESHOLD   -> 503
// - ติดต่อ RPC ไม่ได้เกิน STALL_THRESHOLD -> 502
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type config struct {
	rpcURL         string
	listenPort     string
	checkInterval  time.Duration
	stallThreshold time.Duration
	rpcTimeout     time.Duration
}

type state struct {
	mu          sync.RWMutex
	startedAt   time.Time
	haveBlock   bool
	block       uint64
	lastChange  time.Time // เวลาที่ block เปลี่ยนล่าสุด
	lastSuccess time.Time // เวลาที่เรียก RPC สำเร็จล่าสุด
	lastCheck   time.Time
	lastErr     string
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	v := env(key, "")
	if v == "" {
		return def
	}
	// รองรับทั้ง "30s", "2m" และตัวเลขล้วน (วินาที)
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Fatalf("invalid %s=%q: %v", key, v, err)
	}
	return d
}

func loadConfig() config {
	url := env("RPC_URL", "")
	if url == "" {
		url = fmt.Sprintf("%s://%s:%s",
			env("RPC_SCHEME", "http"),
			env("RPC_HOST", "host.docker.internal"),
			env("RPC_PORT", "8545"))
	}
	return config{
		rpcURL:         url,
		listenPort:     env("LISTEN_PORT", "8080"),
		checkInterval:  envDuration("CHECK_INTERVAL", 15*time.Second),
		stallThreshold: envDuration("STALL_THRESHOLD", 120*time.Second),
		rpcTimeout:     envDuration("RPC_TIMEOUT", 5*time.Second),
	}
}

type rpcResp struct {
	Result string `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func fetchBlock(client *http.Client, url string) (uint64, error) {
	body := []byte(`{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}`)
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("rpc http status %d", resp.StatusCode)
	}
	var r rpcResp
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return 0, fmt.Errorf("decode: %w", err)
	}
	if r.Error != nil {
		return 0, fmt.Errorf("rpc error %d: %s", r.Error.Code, r.Error.Message)
	}
	n, err := strconv.ParseUint(strings.TrimPrefix(r.Result, "0x"), 16, 64)
	if err != nil {
		return 0, fmt.Errorf("parse result %q: %w", r.Result, err)
	}
	return n, nil
}

func (s *state) poll(cfg config, client *http.Client) {
	n, err := fetchBlock(client, cfg.rpcURL)
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastCheck = now
	if err != nil {
		s.lastErr = err.Error()
		log.Printf("check failed: %v", err)
		return
	}
	s.lastErr = ""
	s.lastSuccess = now
	if !s.haveBlock || n != s.block {
		if s.haveBlock && n < s.block {
			log.Printf("block went backwards %d -> %d (reorg/resync?)", s.block, n)
		}
		s.block = n
		s.haveBlock = true
		s.lastChange = now
	}
}

type report struct {
	Status              string  `json:"status"`
	RPC                 string  `json:"rpc"`
	BlockNumber         *uint64 `json:"block_number,omitempty"`
	SecondsSinceChange  *int64  `json:"seconds_since_block_change,omitempty"`
	SecondsSinceSuccess *int64  `json:"seconds_since_rpc_success,omitempty"`
	StallThresholdSec   int64   `json:"stall_threshold_seconds"`
	LastCheck           string  `json:"last_check,omitempty"`
	LastError           string  `json:"last_error,omitempty"`
}

func (s *state) evaluate(cfg config) (int, report) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now()
	r := report{
		RPC:               cfg.rpcURL,
		StallThresholdSec: int64(cfg.stallThreshold.Seconds()),
		LastError:         s.lastErr,
	}
	if !s.lastCheck.IsZero() {
		r.LastCheck = s.lastCheck.UTC().Format(time.RFC3339)
	}
	if s.haveBlock {
		b := s.block
		c := int64(now.Sub(s.lastChange).Seconds())
		ok := int64(now.Sub(s.lastSuccess).Seconds())
		r.BlockNumber, r.SecondsSinceChange, r.SecondsSinceSuccess = &b, &c, &ok
	}

	switch {
	case !s.haveBlock && s.lastErr != "":
		r.Status = "rpc_unreachable"
		return http.StatusBadGateway, r
	case !s.haveBlock:
		r.Status = "starting"
		return http.StatusServiceUnavailable, r
	case now.Sub(s.lastSuccess) > cfg.stallThreshold:
		r.Status = "rpc_unreachable"
		return http.StatusBadGateway, r
	case now.Sub(s.lastChange) > cfg.stallThreshold:
		r.Status = "stalled"
		return http.StatusServiceUnavailable, r
	default:
		r.Status = "syncing"
		return http.StatusOK, r
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func main() {
	healthcheck := flag.Bool("healthcheck", false, "probe the local service (for Docker HEALTHCHECK) and exit")
	flag.Parse()
	cfg := loadConfig()

	if *healthcheck {
		c := &http.Client{Timeout: 3 * time.Second}
		resp, err := c.Get("http://127.0.0.1:" + cfg.listenPort + "/status")
		if err != nil {
			os.Exit(1)
		}
		resp.Body.Close()
		os.Exit(0)
	}

	st := &state{startedAt: time.Now()}
	client := &http.Client{Timeout: cfg.rpcTimeout}

	go func() {
		st.poll(cfg, client)
		t := time.NewTicker(cfg.checkInterval)
		defer t.Stop()
		for range t.C {
			st.poll(cfg, client)
		}
	}()

	mux := http.NewServeMux()
	// /health : status code ตามสถานะ sync (ใช้กับ LB / uptime monitor)
	health := func(w http.ResponseWriter, r *http.Request) {
		code, rep := st.evaluate(cfg)
		writeJSON(w, code, rep)
	}
	mux.HandleFunc("/health", health)
	mux.HandleFunc("/", health)
	// /status : ตอบ 200 เสมอ (ดูข้อมูลอย่างเดียว / Docker healthcheck)
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		_, rep := st.evaluate(cfg)
		writeJSON(w, http.StatusOK, rep)
	})

	log.Printf("rpc-health listening :%s rpc=%s interval=%s stall_threshold=%s",
		cfg.listenPort, cfg.rpcURL, cfg.checkInterval, cfg.stallThreshold)
	srv := &http.Server{
		Addr:              ":" + cfg.listenPort,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}
