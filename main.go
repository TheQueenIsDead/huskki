package main

import (
	"bufio"
	"flag"
	"fmt"
	"html/template"
	"huskki/hub"
	"io"
	"log"
	"math"
	"net/http"
	"strings"

	ds "github.com/starfederation/datastar-go/datastar"
	"go.bug.st/serial"
	"go.bug.st/serial/enumerator"
)

const DEFAULT_BAUD_RATE = 115200

const (
	RPM_DID      = 0x0100
	THROTTLE_DID = 0x0001
	GRIP_DID     = 0x0070
	TPS_DID      = 0x0076
	COOLANT_DID  = 0x0009
)

// Arduino & clones common VIDs
var preferredVIDs = map[string]bool{
	"2341": true, // Arduino
	"2A03": true, // Arduino (older)
	"1A86": true, // CH340
	"10C4": true, // CP210x
	"0403": true, // FTDI
}

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

	eventHub := hub.NewHub()

	go readBinary(serialPort, eventHub)

	dashTemplate, err := template.ParseGlob("*.gohtml")
	if err != nil {
		log.Fatal(err)
	}

	handler := http.NewServeMux()

	// HTTP: index
	handler.HandleFunc("/", func(writer http.ResponseWriter, req *http.Request) {
		err = dashTemplate.ExecuteTemplate(writer, "index", nil)
		if err != nil {
			log.Fatal(err)
		}
	})

	// HTTP: SSE
	handler.HandleFunc("/events", func(writer http.ResponseWriter, req *http.Request) {
		sse := ds.NewSSE(writer, req)

		_, ch, cancel := eventHub.Subscribe()
		defer cancel()

		for {
			if patch(req, ch, sse) {
				return
			}
		}
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

func getArduinoPort(port *string, baud *int, serialPort serial.Port, err error) (serial.Port, error) {
	// auto-select Arduino-ish port if requested
	if *port == "auto" {
		name, err := autoSelectPort()
		if err != nil {
			log.Fatalf("auto-select: %v", err)
		}
		*port = name
	}
	mode := &serial.Mode{BaudRate: *baud}
	serialPort, err = serial.Open(*port, mode)
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
// CRC-8-CCITT over millis..data (excludes magic).
func readBinary(r io.Reader, eventHub *hub.EventHub) {
	br := bufio.NewReader(r)

	for {
		// --- resync on magic ---
		a, err := br.ReadByte()
		if err != nil {
			if err != io.EOF {
				log.Printf("read: %v", err)
			}
			return
		}
		if a != 0xAA {
			continue
		}
		b, err := br.ReadByte()
		if err != nil {
			if err != io.EOF {
				log.Printf("read: %v", err)
			}
			return
		}
		if b != 0x55 {
			continue
		}

		// --- header: ms(4 LE), did(2 BE), len(1) ---
		hdr := make([]byte, 7)
		if _, err := io.ReadFull(br, hdr); err != nil {
			log.Printf("hdr: %v", err)
			return
		}

		did := uint16(hdr[4])<<8 | uint16(hdr[5])
		dl := int(hdr[6])
		if dl < 0 || dl > 64 { // sanity
			log.Printf("bad len %d, resync", dl)
			continue
		}

		// --- payload + crc ---
		payload := make([]byte, dl+1)
		if _, err := io.ReadFull(br, payload); err != nil {
			log.Printf("payload: %v", err)
			return
		}
		data := payload[:dl]
		crcRx := payload[dl]

		// --- verify CRC ---
		crc := crc8UpdateBuf(0x00, hdr[:4]) // millis
		crc = crc8Update(crc, hdr[4])       // did hi
		crc = crc8Update(crc, hdr[5])       // did lo
		crc = crc8Update(crc, hdr[6])       // len
		crc = crc8UpdateBuf(crc, data)      // data
		if crc != crcRx {
			// corrupt frame, drop and resync
			continue
		}

		// hand off raw DID bytes to your existing parser
		broadcastParsedSensorData(eventHub, uint64(did), data)
	}
}

// CRC-8-CCITT, poly 0x07, init 0x00 (matches Arduino writer)
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

func patch(req *http.Request, signalChan <-chan map[string]any, sse *ds.ServerSentEventGenerator) bool {
	select {
	case <-req.Context().Done():
		return true
	case signal, ok := <-signalChan:
		if !ok {
			return true
		}
		var buf []byte
		if v, ok := signal["rpm"]; ok {
			buf = fmt.Appendf(buf, `<span id="rpm">%v</span>`, v)
		}
		if v, ok := signal["throttle"]; ok {
			buf = fmt.Appendf(buf, `<span id="throttle">%v</span>`, v)
		}
		if v, ok := signal["grip"]; ok {
			buf = fmt.Appendf(buf, `<span id="grip">%v</span>`, v)
		}
		if v, ok := signal["tps"]; ok {
			buf = fmt.Appendf(buf, `<span id="tps">%v</span>`, v)
		}
		if v, ok := signal["coolant"]; ok {
			buf = fmt.Appendf(buf, `<span id="coolant">%v</span>`, v)
		}
		if len(buf) > 0 {
			_ = sse.PatchElements(string(buf))
		}
	}
	return false
}

func broadcastParsedSensorData(eventHub *hub.EventHub, didVal uint64, dataBytes []byte) {
	switch uint16(didVal) {
	case RPM_DID: // RPM = u16be / 4
		if len(dataBytes) >= 2 {
			raw := int(dataBytes[0])<<8 | int(dataBytes[1])
			rpm := raw / 4
			eventHub.Broadcast(map[string]any{"rpm": rpm})
		}

	case THROTTLE_DID: // Throttle: (0..255?) no fucking clue what this is smoking, I think this is computed target throttle?
		if len(dataBytes) >= 1 {
			raw8 := int(dataBytes[len(dataBytes)-1])
			//pct := scalePct(raw8, 3, 17) // -> 0..100%
			eventHub.Broadcast(map[string]any{"throttle": raw8})
		}

	case GRIP_DID: // Grip: (0..255) gives raw pot value in percent from the grip (throttle twist)
		if len(dataBytes) >= 1 {
			raw8 := int(dataBytes[len(dataBytes)-1])
			//pct := scalePct(raw8, 20, 59) // -> 0..100%
			eventHub.Broadcast(map[string]any{"grip": raw8})
		}

	case TPS_DID: // TPS (0..1023) -> %
		if len(dataBytes) >= 2 {
			raw := int(dataBytes[0])<<8 | int(dataBytes[1])
			if raw > 1023 {
				raw = 1023
			}
			pct := (raw*100 + 511) / 1023 // integer rounding
			eventHub.Broadcast(map[string]any{"tps": pct})
		}

	case COOLANT_DID: // Coolant °C
		if len(dataBytes) >= 2 {
			val := int(dataBytes[0])<<8 | int(dataBytes[1])
			eventHub.Broadcast(map[string]any{"coolant": val})
		} else if len(dataBytes) == 1 {
			eventHub.Broadcast(map[string]any{"coolant": int(dataBytes[0])})
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
