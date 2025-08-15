package main

import (
	"bufio"
	"fmt"
	"huskki/hub"
	"io"
	"log"
	"strings"
	"time"

	"go.bug.st/serial"
	"go.bug.st/serial/enumerator"
)

// Frame is one validated record from the Arduino stream/log.
type Frame struct {
	Millis uint32 // LE from hdr[0..3]
	DID    uint16 // BE from hdr[4..5]
	Data   []byte // len = hdr[6]
}

const WRITE_EVERY_N_FRAMES = 100

func getArduinoPort(port string, baud int) (serial.Port, error) {
	// auto-select Arduino-ish port if requested
	if port == "auto" {
		name, err := autoSelectPort()
		if err != nil {
			log.Fatalf("auto-select: %v", err)
		}
		port = name
	}
	mode := &serial.Mode{BaudRate: baud}
	serialPort, err := serial.Open(port, mode)
	if err != nil {
		log.Fatalf("couldn't open serial %s: %v", port, err)
	}
	log.Printf("connected to %s @ %d", port, baud)

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

// readBinary consumes binary can frames with layout:
// [AA 55][millis:u32 LE][DID:u16 BE][len:u8][data:len][crc8:u8]
func readBinary(r io.Reader, eventHub *hub.EventHub, raw *bufio.Writer) {
	br := bufio.NewReader(r)
	frames := 0

	for {
		fr, err := readOneFrame(br)
		if err != nil {
			if err != io.EOF {
				log.Printf("read frame: %v", err)
				continue
			}
			return
		}

		// ---- SAVE the exact validated frame (magic..crc) ----
		if raw != nil {
			// rebuild exact record
			dl := len(fr.Data)
			rec := make([]byte, 2+7+dl+1)
			rec[0], rec[1] = 0xAA, 0x55

			// header
			m := fr.Millis
			rec[2] = byte(m)
			rec[3] = byte(m >> 8)
			rec[4] = byte(m >> 16)
			rec[5] = byte(m >> 24)
			rec[6] = byte(fr.DID >> 8)
			rec[7] = byte(fr.DID)
			rec[8] = byte(dl)

			// payload
			copy(rec[9:9+dl], fr.Data)

			// crc (recompute exactly like logger)
			crc := crc8UpdateBuf(0x00, rec[2:6])  // millis
			crc = crc8Update(crc, rec[6])         // did hi
			crc = crc8Update(crc, rec[7])         // did lo
			crc = crc8Update(crc, rec[8])         // len
			crc = crc8UpdateBuf(crc, rec[9:9+dl]) // payload
			rec[9+dl] = crc

			if _, err := raw.Write(rec); err != nil {
				log.Printf("raw write: %v", err)
			} else {
				frames++
				if (frames % WRITE_EVERY_N_FRAMES) == 0 {
					_ = raw.Flush()
				}
			}
		}

		// hand off parsed bytes (keep your current wall-clock stamp here)
		BroadcastParsedSensorData(eventHub, uint64(fr.DID), fr.Data, int(time.Now().UnixMilli()))
	}
}

// readOneFrame reads a single frame with layout:
// [AA 55][millis:u32 LE][DID:u16 BE][len:u8][data:len][crc8]
func readOneFrame(br *bufio.Reader) (Frame, error) {
	var z Frame

	// resync on magic AA 55
	for {
		a, err := br.ReadByte()
		if err != nil {
			return z, err
		}
		if a != 0xAA {
			continue
		}
		b, err := br.ReadByte()
		if err != nil {
			return z, err
		}
		if b == 0x55 {
			break
		}
		// otherwise keep scanning
	}

	// header: millis(4 LE) + did(2 BE) + len(1)
	hdr := make([]byte, 7)
	if _, err := io.ReadFull(br, hdr); err != nil {
		return z, err
	}
	dl := int(hdr[6])
	if dl < 0 || dl > 64 {
		// TODO: replace with var
		return z, fmt.Errorf("error bad len: %d", dl)
	}

	// payload + crc
	tail := make([]byte, dl+1)
	if _, err := io.ReadFull(br, tail); err != nil {
		return z, err
	}
	data := tail[:dl]
	crcRx := tail[dl]

	// verify CRC over: millis(4) + did_hi + did_lo + len + data
	crc := crc8UpdateBuf(0x00, hdr[:4]) // millis
	crc = crc8Update(crc, hdr[4])       // did hi
	crc = crc8Update(crc, hdr[5])       // did lo
	crc = crc8Update(crc, hdr[6])       // len
	crc = crc8UpdateBuf(crc, data)      // payload
	if crc != crcRx {
		// TODO: replace with var, also re enable this
		//return z, fmt.Errorf("error bad crc")
	}

	// parse fields
	millis := uint32(hdr[0]) |
		uint32(hdr[1])<<8 |
		uint32(hdr[2])<<16 |
		uint32(hdr[3])<<24
	did := uint16(hdr[4])<<8 | uint16(hdr[5])

	return Frame{
		Millis: millis,
		DID:    did,
		Data:   append([]byte(nil), data...), // copy
	}, nil
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
