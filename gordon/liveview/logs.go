package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// LogLine is a single parsed line from a service log file.
type LogLine struct {
	Service string `json:"service"`
	Level   string `json:"level"`
	Ts      string `json:"ts"`
	Msg     string `json:"msg"`
	Raw     string `json:"raw"`
}

var (
	logStreamClients   []chan string
	logStreamClientsMu sync.Mutex
)

func broadcastLogLine(ll LogLine) {
	data, err := json.Marshal(ll)
	if err != nil {
		return
	}
	logStreamClientsMu.Lock()
	defer logStreamClientsMu.Unlock()
	for _, ch := range logStreamClients {
		select {
		case ch <- string(data):
		default:
		}
	}
}

// parseServiceLogLine splits a standard Go log line into its parts.
// Format: "2006/01/02 15:04:05 [LEVEL] message" (19-char prefix).
func parseServiceLogLine(service, raw string) LogLine {
	ll := LogLine{Service: service, Raw: raw}
	if len(raw) < 20 {
		ll.Msg = raw
		return ll
	}
	ll.Ts = raw[:19]
	rest := strings.TrimSpace(raw[19:])
	if strings.HasPrefix(rest, "[") {
		if end := strings.Index(rest, "]"); end > 0 {
			ll.Level = rest[1:end]
			ll.Msg = strings.TrimSpace(rest[end+1:])
			return ll
		}
	}
	ll.Msg = rest
	return ll
}

// startLogTailer polls all non-build *.log files in logDir every 300 ms and
// broadcasts new lines to connected SSE clients. Runs forever as a goroutine.
func startLogTailer(logDir string) {
	if logDir == "" {
		return
	}
	offsets := make(map[string]int64)

	for {
		entries, err := os.ReadDir(logDir)
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".log") || strings.HasPrefix(name, "build-") {
				continue
			}
			path := filepath.Join(logDir, name)
			service := strings.TrimSuffix(name, ".log")

			f, err := os.Open(path)
			if err != nil {
				continue
			}
			info, err := f.Stat()
			if err != nil {
				f.Close()
				continue
			}
			if _, seen := offsets[path]; !seen {
				// First encounter: anchor at the current end so we only tail from now.
				offsets[path] = info.Size()
				f.Close()
				continue
			}
			if info.Size() < offsets[path] {
				offsets[path] = 0 // file was rotated / truncated
			}
			f.Seek(offsets[path], 0) //nolint:errcheck
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				if line := sc.Text(); line != "" {
					broadcastLogLine(parseServiceLogLine(service, line))
				}
			}
			pos, _ := f.Seek(0, 1)
			offsets[path] = pos
			f.Close()
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// handleLogsStream is an SSE endpoint: connects and receives live log lines.
func handleLogsStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	ch := make(chan string, 100)
	logStreamClientsMu.Lock()
	logStreamClients = append(logStreamClients, ch)
	logStreamClientsMu.Unlock()

	defer func() {
		logStreamClientsMu.Lock()
		for i, c := range logStreamClients {
			if c == ch {
				logStreamClients = append(logStreamClients[:i], logStreamClients[i+1:]...)
				break
			}
		}
		logStreamClientsMu.Unlock()
	}()

	fmt.Fprint(w, "event: open\ndata: connected\n\n")
	flusher.Flush()

	hb := time.NewTicker(20 * time.Second)
	defer hb.Stop()
	for {
		select {
		case msg := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
		case <-hb.C:
			fmt.Fprint(w, ": hb\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// handleLogsSnapshot returns the last ~50 lines from each non-build log file,
// merged and sorted by timestamp, capped at 300 total. Used for initial load.
func handleLogsSnapshot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	logDir := getEnv("LOG_DIR", "")
	if logDir == "" {
		w.Write([]byte("[]")) //nolint:errcheck
		return
	}

	const perFile = 50
	var all []LogLine

	entries, _ := os.ReadDir(logDir)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".log") || strings.HasPrefix(name, "build-") {
			continue
		}
		service := strings.TrimSuffix(name, ".log")
		data, err := os.ReadFile(filepath.Join(logDir, name))
		if err != nil {
			continue
		}
		rawLines := strings.Split(string(data), "\n")
		start := len(rawLines) - perFile
		if start < 0 {
			start = 0
		}
		for _, l := range rawLines[start:] {
			if l != "" {
				all = append(all, parseServiceLogLine(service, l))
			}
		}
	}

	sort.Slice(all, func(i, j int) bool { return all[i].Ts < all[j].Ts })
	if len(all) > 300 {
		all = all[len(all)-300:]
	}
	json.NewEncoder(w).Encode(all) //nolint:errcheck
}
