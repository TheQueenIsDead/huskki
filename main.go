package main

import (
	"bufio"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"

	ds "github.com/starfederation/datastar/sdk/go"
	"go.bug.st/serial"
	"go.bug.st/serial/enumerator"
)

type hub struct {
	mu   sync.Mutex
	subs map[int]chan map[string]any
	next int
	last map[string]any
}

func newHub() *hub { return &hub{subs: map[int]chan map[string]any{}, last: map[string]any{}} }

func (h *hub) subscribe() (int, <-chan map[string]any, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	id := h.next
	h.next++
	ch := make(chan map[string]any, 16)
	// send current snapshot on subscribe
	if len(h.last) > 0 {
		ch <- h.copy(h.last)
	}
	h.subs[id] = ch
	cancel := func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if c, ok := h.subs[id]; ok {
			close(c)
			delete(h.subs, id)
		}
	}
	return id, ch, cancel
}

func (h *hub) copy(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (h *hub) broadcast(sig map[string]any) {
	h.mu.Lock()
	for k, v := range sig {
		h.last[k] = v
	}
	for _, ch := range h.subs {
		select { // non-blocking
		case ch <- h.copy(sig):
		default:
		}
	}
	h.mu.Unlock()
}

func main() {
	port := flag.String("port", "auto", "serial device path or 'auto'")
	baud := flag.Int("baud", 115200, "baud rate (ignored when -port=auto)")
	addr := flag.String("addr", ":8080", "http listen address")
	flag.Parse()

	// open serial (auto-select Arduino-like USB port @ 115200 if -port=auto)
	if *port == "auto" {
		name, err := autoSelectPort()
		if err != nil {
			log.Fatalf("auto-select: %v", err)
		}
		*port = name
		*baud = 115200
	}
	mode := &serial.Mode{BaudRate: *baud}
	s, err := serial.Open(*port, mode)
	if err != nil {
		log.Fatalf("open serial %s: %v", *port, err)
	}
	log.Printf("Connected to %s @ %d", *port, *baud)
	defer func(s serial.Port) {
		err = s.Close()
		if err != nil {
			log.Fatalf("close serial: %v", err)
		}
	}(s)

	h := newHub()

	// serial reader
	go func() {
		sc := bufio.NewScanner(s)
		// handle long lines if needed
		buf := make([]byte, 0, 64*1024)
		sc.Buffer(buf, 1024*1024)

		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			// Expect: millis,DID,data_hex[,u16be...]
			parts := strings.SplitN(line, ",", 4)
			if len(parts) < 3 {
				continue
			}
			didStr := parts[1]
			if !strings.HasPrefix(didStr, "0x") {
				continue
			}
			didVal, err := strconv.ParseUint(didStr[2:], 16, 16)
			if err != nil {
				continue
			}
			dataHex := parts[2]
			// spaces like "12 34 56" -> "123456"
			clean := strings.ReplaceAll(dataHex, " ", "")
			if len(clean)%2 == 1 {
				continue
			}
			b, err := hex.DecodeString(clean)
			if err != nil || len(b) == 0 {
				continue
			}

			switch uint16(didVal) {
			case 0x0100: // RPM: big-endian word / 4
				if len(b) >= 2 {
					raw := int(b[0])<<8 | int(b[1])
					rpm := raw / 4
					h.broadcast(map[string]any{"rpm": rpm})
				}
			}
			// add more DIDs later: coolant, TPS, etc.
		}
		if err := sc.Err(); err != nil {
			log.Printf("serial scanner error: %v", err)
		}
	}()

	// HTTP: index
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/html; charset=utf-8")
		_, err = fmt.Fprint(w, indexHTML)
		if err != nil {
			log.Printf("write index.html: %v", err)
		}
	})

	// HTTP: SSE events
	http.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		sse := ds.NewSSE(w, r)

		_, ch, cancel := h.subscribe()
		defer cancel()

		// send initial snapshot handled by subscribe()

		for {
			select {
			case <-r.Context().Done():
				return
			case sig, ok := <-ch:
				if !ok {
					return
				}
				// MergeSignals patches named signals in the client store via SSE.
				if err := sse.MarshalAndMergeSignals(sig); err != nil {
					return
				}
			}
		}
	})

	log.Printf("Listening on %s …", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

// --- auto-detect a likely Arduino/clone serial device ---
var preferredVIDs = map[string]bool{
	"2341": true, // Arduino
	"2A03": true, // Arduino (older)
	"1A86": true, // CH340
	"10C4": true, // CP210x
	"0403": true, // FTDI
}

func autoSelectPort() (string, error) {
	ports, err := enumerator.GetDetailedPortsList()
	if err != nil {
		return "", fmt.Errorf("enumerate ports: %w", err)
	}
	// Prefer known Arduino/clone VIDs
	for _, p := range ports {
		if p.IsUSB && preferredVIDs[strings.ToUpper(p.VID)] {
			return p.Name, nil
		}
	}
	// Fallback: any USB serial
	for _, p := range ports {
		if p.IsUSB {
			return p.Name, nil
		}
	}
	if len(ports) > 0 {
		return ports[0].Name, nil
	}
	return "", fmt.Errorf("no serial ports found")
}

const indexHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8" />
<meta name="viewport" content="width=device-width, initial-scale=1" />
<title>ECU Live RPM</title>
<!-- Datastar client -->
<script type="module" src="https://cdn.jsdelivr.net/gh/starfederation/datastar@v0.21.4/bundles/datastar.js"></script>
<style>
  body { font-family: system-ui, -apple-system, Segoe UI, Roboto, sans-serif; margin: 2rem; }
  .card { display:inline-block; padding:1.25rem 1.5rem; border-radius:14px; box-shadow:0 8px 24px rgba(0,0,0,.08); }
  .label { color:#666; font-size:.9rem; }
  .value { font-size:3rem; font-weight:700; letter-spacing:.02em; }
  .unit { font-size:1.1rem; color:#777; padding-left:.25rem; }
</style>
</head>
<body>
  <!-- Establish SSE connection on load -->
  <div data-on-load="@get('/events', {openWhenHidden: true})"></div>

  <div class="card">
    <div class="label">RPM</div>
    <div class="value">
      <span data-text="$rpm ?? '—'"></span><span class="unit">rpm</span>
    </div>
  </div>
</body>
</html>`
