package main

import (
	"bufio"
	_ "embed"
	"flag"
	"fmt"
	"html/template"
	"huskki/hub"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	ds "github.com/starfederation/datastar-go/datastar"
	"go.bug.st/serial"
	"go.bug.st/serial/enumerator"
)

// Config
const (
	DEFAULT_BAUD_RATE = 115200
	WRITE_EVERY_N_FRAMES = 100
)

// Known DIDs
const (
	RPM_DID      = 0x0100
	THROTTLE_DID = 0x0001
	GRIP_DID     = 0x0070
	TPS_DID      = 0x0076
	COOLANT_DID  = 0x0009
)

//go:embed static/datastar.js
var datastarJS []byte

// Arduino & clones common VIDs
var preferredVIDs = map[string]bool{
	"2341": true, // Arduino
	"2A03": true, // Arduino (older)
	"1A86": true, // CH340
	"10C4": true, // CP210x
	"0403": true, // FTDI
}

type GraphData struct {
	X int
	Y int
}

// Globals
var (
	Templates *template.Template
	EventHub  *hub.EventHub
)

func main() {
	port, baud, addr, replayFile := getFlags()

	isReplay := *replayFile != ""

	var serialPort serial.Port
	var err error
	if !isReplay {
		serialPort, err = getArduinoPort(port, baud, serialPort, err)
		defer func() {
			if err := serialPort.Close(); err != nil {
				log.Printf("close serial: %v", err)
			}
		}()
	}

	EventHub = hub.NewHub()

	// TODO, support replays
	var rawW *bufio.Writer
	f, err := os.OpenFile("/home/kees/rawlog", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Fatalf("open rawlog: %v", err)
	}
	defer f.Close()
	rawW = bufio.NewWriterSize(f, 1<<20) // 1MB buffer
	defer rawW.Flush()
	go readBinary(serialPort, eventHub, rawW)

	// Initialise HTML templating
	Templates = template.New("").Funcs(template.FuncMap{
		"ToLower": strings.ToLower,
	})
	Templates, err = Templates.ParseGlob("templates/*.gohtml")
	if err != nil {
		log.Fatal(err)
	}

	handler := http.NewServeMux()
	handler.HandleFunc("/", IndexHandler)
	handler.HandleFunc("/events", EventsHandler)

	// TODO: extract to func
	handler.HandleFunc("/datastar.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Write(datastarJS)
	})

	log.Printf("Listening on %s …", *addr)
	log.Fatal(http.ListenAndServe(*addr, handler))
}

func getFlags() (*string, *int, *string, *string) {
	port := flag.String("port", "auto", "serial device path or 'auto'")
	baud := flag.Int("baud", DEFAULT_BAUD_RATE, "baud rate")
	addr := flag.String("addr", ":8080", "http listen address")
	replayFile := flag.String("replay", "", "path to replay file (csv log)")
	flag.Parse()
	return port, baud, addr, replayFile
}

func getArduinoPort(port *string, baud *int) (serial.Port, error) {
	// auto-select Arduino-ish port if requested
	if *port == "auto" {
		name, err := autoSelectPort()
		if err != nil {
			log.Fatalf("auto-select: %v", err)
		}
		*port = name
	}
	mode := &serial.Mode{BaudRate: *baud}
	serialPort, err := serial.Open(*port, mode)
	if err != nil {
		log.Fatalf("open serial %s: %v", *port, err)
	}
	log.Printf("Connected to %s @ %d", *port, *baud)

	return serialPort, err
}

func autoSelectPort() (string, error) {
	ports, err := enumerator.GetDetailedPortsList()
	if err != nil {
		return "", fmt.Errorf("enumerate ports: %w", err)
	}
	for _, p := range ports {
		if p.IsUSB && preferredVIDs[strings.ToUpper(p.VID)] {
			return p.Name, nil
		}
	}
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

// readBinary consumes records with layout:
// [AA 55][millis:u32 LE][DID:u16 BE][len:u8][data:len][crc8:u8]
func readBinary(r io.Reader, eventHub *hub.EventHub, raw *bufio.Writer) {
	br := bufio.NewReader(r)
	frames := 0

	for {
		// resync on magic
		a, err := br.ReadByte()
		if err != nil {
			if err != io.EOF {
				log.Printf("read: %v", err)
			}
			continue
		}
		if a != 0xAA {
			continue
		}
		b, err := br.ReadByte()
		if err != nil {
			if err != io.EOF {
				log.Printf("read: %v", err)
			}
			continue
		}
		if b != 0x55 {
			continue
		}

		// header
		hdr := make([]byte, 7)
		if _, err := io.ReadFull(br, hdr); err != nil {
			log.Printf("hdr: %v", err)
			continue
		}
		did := uint16(hdr[4])<<8 | uint16(hdr[5])
		dl := int(hdr[6])
		if dl < 0 || dl > 64 {
			continue
		}

		// payload + crc
		tail := make([]byte, dl+1)
		if _, err := io.ReadFull(br, tail); err != nil {
			log.Printf("payload: %v", err)
			continue
		}
		data := tail[:dl]
		crcRx := tail[dl]

		// verify CRC
		crc := crc8UpdateBuf(0x00, hdr[:4]) // millis
		crc = crc8Update(crc, hdr[4])       // did hi
		crc = crc8Update(crc, hdr[5])       // did lo
		crc = crc8Update(crc, hdr[6])       // len
		crc = crc8UpdateBuf(crc, data)      // payload
		if crc != crcRx {
			continue // drop bad frame
		}

		// ---- SAVE the exact validated frame (magic .. crc) ----
		if raw != nil {
			// Build one contiguous record and append
			rec := make([]byte, 2+7+dl+1)
			rec[0], rec[1] = 0xAA, 0x55
			copy(rec[2:9], hdr)     // millis(4) + did(2) + len(1)
			copy(rec[9:9+dl], data) // payload
			rec[9+dl] = crcRx       // crc
			if _, err := raw.Write(rec); err != nil {
				log.Printf("raw write: %v", err)
			} else {
				frames++
				if writeEvery > 0 && (frames%writeEvery) == 0 {
					_ = raw.Flush()
				}
			}
		}

		// hand off parsed bytes
		broadcastParsedSensorData(eventHub, uint64(did), data, int(time.Now().UnixMilli()))
	}
}

// CRC-8-CCITT helpers (poly 0x07, init 0x00)
func crc8Update(crc, b byte) byte {
	crc ^= b
	for i := 0; i < 8; i++ {
		if crc&0x80 != 0 {
			crc = (crc << 1) ^ 0x07
		} else {
			crc <<= 1
		}
	}
	return crc
}
func crc8UpdateBuf(crc byte, p []byte) byte {
	for _, b := range p {
		crc = crc8Update(crc, b)
	}
	return crc
}

func broadcastParsedSensorData(eventHub *hub.EventHub, didVal uint64, dataBytes []byte, timestamp int) {
	switch uint16(didVal) {
	case RPM_DID: // RPM = u16be / 4
		if len(dataBytes) >= 2 {
			raw := int(dataBytes[0])<<8 | int(dataBytes[1])
			rpm := raw / 4
			eventHub.Broadcast(map[string]any{"rpm": rpm, "timestamp": timestamp})
		}

	case THROTTLE_DID: // Throttle: (0..255?) no fucking clue what this is smoking, I think this is computed target throttle?
		if len(dataBytes) >= 1 {
			raw8 := int(dataBytes[len(dataBytes)-1])
			//pct := scalePct(raw8, 3, 17) // -> 0..100%
			eventHub.Broadcast(map[string]any{"throttle": raw8, "timestamp": timestamp})
		}

	case GRIP_DID: // Grip: (0..255) gives raw pot value in percent from the grip (throttle twist)
		if len(dataBytes) >= 1 {
			raw8 := int(dataBytes[len(dataBytes)-1])
			//pct := scalePct(raw8, 20, 59) // -> 0..100%
			eventHub.Broadcast(map[string]any{"grip": raw8, "timestamp": timestamp})
		}

	case TPS_DID: // TPS (0..1023) -> %
		if len(dataBytes) >= 2 {
			raw := int(dataBytes[0])<<8 | int(dataBytes[1])
			if raw > 1023 {
				raw = 1023
			}
			pct := (raw*100 + 511) / 1023 // integer rounding
			eventHub.Broadcast(map[string]any{"tps": pct, "timestamp": timestamp})
		}

	case COOLANT_DID: // Coolant °C
		if len(dataBytes) >= 2 {
			val := int(dataBytes[0])<<8 | int(dataBytes[1])
			eventHub.Broadcast(map[string]any{"coolant": val - 40, "timestamp": timestamp})
		} else if len(dataBytes) == 1 {
			eventHub.Broadcast(map[string]any{"coolant": int(dataBytes[0]) - 40, "timestamp": timestamp})
		}
	}
}

func scalePct(raw, min, max int) int {
	if max <= min {
		return 0
	}
	if raw < min {
		raw = min
	}
	if raw > max {
		raw = max
	}
	return int(math.Round(float64(raw-min) * 100.0 / float64(max-min)))
}
